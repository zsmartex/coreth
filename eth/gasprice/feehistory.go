// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	_ "github.com/zsmartex/coreth/consensus/misc"
	"github.com/zsmartex/coreth/core/types"
	"github.com/zsmartex/coreth/rpc"
)

var (
	errInvalidPercentile     = errors.New("invalid reward percentile")
	errRequestBeyondHead     = errors.New("request beyond head block")
	errBeyondHistoricalLimit = errors.New("request beyond historical limit")
)

const (
	// maxBlockFetchers is the max number of goroutines to spin up to pull blocks
	// for the fee history calculation (mostly relevant for LES).
	maxBlockFetchers = 4
)

// blockFees represents a single block for processing
type blockFees struct {
	// set by the caller
	blockNumber uint64
	// filled by processBlock
	results processedFees
	err     error
}

// processedFees contains the results of a processed block and is also used for caching
type processedFees struct {
	reward       []*big.Int
	baseFee      *big.Int
	gasUsedRatio float64
}

// txGasAndReward is sorted in ascending order based on reward
type (
	txGasAndReward struct {
		gasUsed uint64
		reward  *big.Int
	}
	sortGasAndReward []txGasAndReward
	slimBlock        struct {
		GasUsed  uint64
		GasLimit uint64
		BaseFee  *big.Int
		Txs      []txGasAndReward
	}
)

func (s sortGasAndReward) Len() int { return len(s) }
func (s sortGasAndReward) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s sortGasAndReward) Less(i, j int) bool {
	return s[i].reward.Cmp(s[j].reward) < 0
}

// processBlock prepares a [slimBlock] from a retrieved block and list of
// receipts. This slimmed block can be cached and used for future calls.
func processBlock(block *types.Block, receipts types.Receipts) *slimBlock {
	var sb slimBlock
	if sb.BaseFee = block.BaseFee(); sb.BaseFee == nil {
		sb.BaseFee = new(big.Int)
	}
	sb.GasUsed = block.GasUsed()
	sb.GasLimit = block.GasLimit()
	sorter := make(sortGasAndReward, len(block.Transactions()))
	for i, tx := range block.Transactions() {
		reward, _ := tx.EffectiveGasTip(sb.BaseFee)
		sorter[i] = txGasAndReward{gasUsed: receipts[i].GasUsed, reward: reward}
	}
	sort.Sort(sorter)
	sb.Txs = sorter
	return &sb
}

// processPercentiles returns a [processedFees] object with a populated
// baseFee, gasUsedRatio, and optionally reward percentiles (if any are
// requested)
func (sb *slimBlock) processPercentiles(percentiles []float64) processedFees {
	var results processedFees
	results.baseFee = sb.BaseFee // already set to be non-nil
	results.gasUsedRatio = float64(sb.GasUsed) / float64(sb.GasLimit)
	if len(percentiles) == 0 {
		// rewards were not requested
		return results
	}

	txLen := len(sb.Txs)
	results.reward = make([]*big.Int, len(percentiles))
	if txLen == 0 {
		// return an all zero row if there are no transactions to gather data from
		for i := range results.reward {
			results.reward[i] = new(big.Int)
		}
		return results
	}

	// sb transactions are already sorted by tip, so we don't need to re-sort
	var txIndex int
	sumGasUsed := sb.Txs[0].gasUsed
	for i, p := range percentiles {
		thresholdGasUsed := uint64(float64(sb.GasUsed) * p / 100)
		for sumGasUsed < thresholdGasUsed && txIndex < txLen-1 {
			txIndex++
			sumGasUsed += sb.Txs[txIndex].gasUsed
		}
		results.reward[i] = sb.Txs[txIndex].reward
	}
	return results
}

// resolveBlockRange resolves the specified block range to absolute block numbers while also
// enforcing backend specific limitations.
// Note: an error is only returned if retrieving the head header has failed. If there are no
// retrievable blocks in the specified range then zero block count is returned with no error.
func (oracle *Oracle) resolveBlockRange(ctx context.Context, lastBlock rpc.BlockNumber, blocks int) (uint64, int, error) {
	// Query either pending block or head header and set headBlock
	if lastBlock == rpc.PendingBlockNumber {
		// Pending block not supported by backend, process until latest block
		lastBlock = rpc.LatestBlockNumber
		blocks--
	}
	if blocks == 0 {
		return 0, 0, nil
	}

	lastAcceptedBlock := rpc.BlockNumber(oracle.backend.LastAcceptedBlock().NumberU64())
	maxQueryDepth := rpc.BlockNumber(oracle.maxBlockHistory) - 1
	if lastBlock.IsAccepted() {
		lastBlock = lastAcceptedBlock
	} else if lastAcceptedBlock > maxQueryDepth && lastAcceptedBlock-maxQueryDepth > lastBlock {
		// If the requested last block reaches further back than [oracle.maxBlockHistory] past the last accepted block return an error
		// Note: this allows some blocks past this point to be fetched since it will start fetching [blocks] from this point.
		return 0, 0, fmt.Errorf("%w: requested %d, head %d", errBeyondHistoricalLimit, lastBlock, lastAcceptedBlock)
	} else if lastBlock > lastAcceptedBlock {
		// If the requested block is above the accepted block return an error
		return 0, 0, fmt.Errorf("%w: requested %d, head %d", errRequestBeyondHead, lastBlock, lastAcceptedBlock)
	}
	// Ensure not trying to retrieve before genesis
	if rpc.BlockNumber(blocks) > lastBlock+1 {
		blocks = int(lastBlock + 1)
	}
	// Truncate blocks range if extending past [oracle.maxBlockHistory]
	oldestQueriedIndex := lastBlock - rpc.BlockNumber(blocks) + 1
	if queryDepth := lastAcceptedBlock - oldestQueriedIndex; queryDepth > maxQueryDepth {
		overage := int(queryDepth - maxQueryDepth)
		blocks -= overage
	}
	// It is not possible that [blocks] could be <= 0 after
	// truncation as the [lastBlock] requested will at least by fetchable.
	// Otherwise, we would've returned an error earlier.
	return uint64(lastBlock), blocks, nil
}

// FeeHistory returns data relevant for fee estimation based on the specified range of blocks.
// The range can be specified either with absolute block numbers or ending with the latest
// or pending block. Backends may or may not support gathering data from the pending block
// or blocks older than a certain age (specified in maxHistory). The first block of the
// actually processed range is returned to avoid ambiguity when parts of the requested range
// are not available or when the head has changed during processing this request.
// Three arrays are returned based on the processed blocks:
// - reward: the requested percentiles of effective priority fees per gas of transactions in each
//   block, sorted in ascending order and weighted by gas used.
// - baseFee: base fee per gas in the given block
// - gasUsedRatio: gasUsed/gasLimit in the given block
// Note: baseFee includes the next block after the newest of the returned range, because this
// value can be derived from the newest block.
func (oracle *Oracle) FeeHistory(ctx context.Context, blocks int, unresolvedLastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	if blocks < 1 {
		return common.Big0, nil, nil, nil, nil // returning with no data and no error means there are no retrievable blocks
	}
	if blocks > oracle.maxCallBlockHistory {
		log.Warn("Sanitizing fee history length", "requested", blocks, "truncated", oracle.maxCallBlockHistory)
		blocks = oracle.maxCallBlockHistory
	}
	for i, p := range rewardPercentiles {
		if p < 0 || p > 100 {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: %f", errInvalidPercentile, p)
		}
		if i > 0 && p < rewardPercentiles[i-1] {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: #%d:%f > #%d:%f", errInvalidPercentile, i-1, rewardPercentiles[i-1], i, p)
		}
	}
	lastBlock, blocks, err := oracle.resolveBlockRange(ctx, unresolvedLastBlock, blocks)
	if err != nil || blocks == 0 {
		return common.Big0, nil, nil, nil, err
	}
	oldestBlock := lastBlock + 1 - uint64(blocks)

	var (
		next    = oldestBlock
		results = make(chan *blockFees, blocks)
	)
	for i := 0; i < maxBlockFetchers && i < blocks; i++ {
		go func() {
			for {
				// Retrieve the next block number to fetch with this goroutine
				blockNumber := atomic.AddUint64(&next, 1) - 1
				if blockNumber > lastBlock {
					return
				}

				fees := &blockFees{blockNumber: blockNumber}
				var sb *slimBlock
				if sbRaw, ok := oracle.historyCache.Get(blockNumber); ok {
					sb = sbRaw.(*slimBlock)
				} else {
					block, err := oracle.backend.BlockByNumber(ctx, rpc.BlockNumber(blockNumber))
					if block == nil || err != nil {
						fees.err = err
						results <- fees
						return
					}
					receipts, err := oracle.backend.GetReceipts(ctx, block.Hash())
					if err != nil {
						fees.err = err
						results <- fees
						return
					}
					sb = processBlock(block, receipts)
					oracle.historyCache.Add(blockNumber, sb)
				}
				fees.results = sb.processPercentiles(rewardPercentiles)
				results <- fees
			}
		}()
	}
	var (
		reward       = make([][]*big.Int, blocks)
		baseFee      = make([]*big.Int, blocks)
		gasUsedRatio = make([]float64, blocks)
		firstMissing = blocks
	)
	for ; blocks > 0; blocks-- {
		fees := <-results
		if fees.err != nil {
			return common.Big0, nil, nil, nil, fees.err
		}
		i := int(fees.blockNumber - oldestBlock)
		if fees.results.baseFee != nil {
			reward[i], baseFee[i], gasUsedRatio[i] = fees.results.reward, fees.results.baseFee, fees.results.gasUsedRatio
		} else {
			// getting no block and no error means we are requesting into the future (might happen because of a reorg)
			if i < firstMissing {
				firstMissing = i
			}
		}
	}
	if firstMissing == 0 {
		return common.Big0, nil, nil, nil, nil
	}
	if len(rewardPercentiles) != 0 {
		reward = reward[:firstMissing]
	} else {
		reward = nil
	}
	baseFee, gasUsedRatio = baseFee[:firstMissing], gasUsedRatio[:firstMissing]
	return new(big.Int).SetUint64(oldestBlock), reward, baseFee, gasUsedRatio, nil
}
