// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package pamutil

import (
	"context"
	"fmt"
	"runtime"

	"github.com/grailbio/base/errors"
	"github.com/grailbio/base/file"
	"github.com/grailbio/base/log"
	"github.com/grailbio/base/recordio"
	"github.com/grailbio/bio/biopb"
)

type readSubshard struct {
	shardRange  biopb.CoordRange           // Rowshard range.
	approxBytes int64                      // Approx #bytes in the range.
	blocks      []biopb.PAMBlockIndexEntry // Sample of block indexes in the range.
}

func validateFieldIndex(index biopb.PAMFieldIndex) error {
	for _, block := range index.Blocks {
		if block.NumRecords == 0 {
			return fmt.Errorf("corrupt block index: %+v", block)
		}
	}
	return nil
}

// Get the size of file for given rowshard and field.
func fieldFileSize(ctx context.Context, dir string, recRange biopb.CoordRange, field string) int64 {
	path := FieldDataPath(dir, recRange, field)
	stat, err := file.Stat(ctx, path)
	if err != nil {
		log.Printf("stat %v: %v", path, err)
		return 0
	}
	return stat.Size()
}

// readFieldIndex reads the index for, "dir/recRange.field".
func readFieldIndex(ctx context.Context, dir string, recRange biopb.CoordRange, field string) (index biopb.PAMFieldIndex, err error) {
	path := FieldDataPath(dir, recRange, field)
	in, err := file.Open(ctx, path)
	if err != nil {
		return index, err
	}
	defer file.CloseAndReport(ctx, in, &err)
	rio := recordio.NewScanner(in.Reader(ctx), recordio.ScannerOpts{})
	trailer := rio.Trailer()
	if len(trailer) == 0 {
		return index, errors.E(err, fmt.Sprintf("readfieldindex %v: file does not contain an index", path))
	}
	if err := index.Unmarshal(trailer); err != nil {
		return index, errors.E(err, fmt.Sprintf("%v: unmarshal field index for field '%s'", path, field))
	}
	err = validateFieldIndex(index)
	if e := rio.Finish(); e != nil && err == nil {
		err = e
	}
	return index, err
}

// Read *.index files listed in "files", then narrow their ShardIndex.Blocks so
// that they only contains blocks that intersect recRange.

func readAndSubsetIndexes(ctx context.Context, files []FileInfo, recRange biopb.CoordRange, fields []string) ([]readSubshard, error) {
	// Extract a subset of "blocks" that intersect with
	// requestedRange. shardLimit is the limit of the shard file.
	intersectIndexBlocks := func(
		blocks []biopb.PAMBlockIndexEntry, shardLimit biopb.Coord,
		requestedRange biopb.CoordRange) []biopb.PAMBlockIndexEntry {
		result := []biopb.PAMBlockIndexEntry{}
		for _, block := range blocks {
			if BlockIntersectsRange(block.StartAddr, block.EndAddr, requestedRange) {
				result = append(result, block)
			} else {
				log.Printf("ReadAndSubset: shardlimit: %+v, reqRange %+v drop block %+v", shardLimit, requestedRange, block)
			}
		}
		return result
	}

	indexes := make([]readSubshard, 0, len(files))
	for _, indexFile := range files {
		// Below, we pick an arbitrary field obtain a sample of record coordinates
		// and corresponding file offsets. We pick the largeest field, which will
		// have the largest # of coordinates and offsets to sample from.
		fieldFileSizes := map[string]int64{}
		sampledFieldSize := int64(-1)
		totalFileBytes := int64(0)
		var sampledField string
		for _, field := range fields {
			size := fieldFileSize(ctx, indexFile.Dir, indexFile.Range, field)
			fieldFileSizes[field] = size
			if size > sampledFieldSize {
				sampledField = field
				sampledFieldSize = size
			}
			totalFileBytes += size
		}
		index, err := readFieldIndex(ctx, indexFile.Dir, indexFile.Range, sampledField)
		if err != nil {
			log.Panicf("%+v: failed to read index: %v", indexFile, err)
			return nil, err
		}
		log.Debug.Printf("Read index: %+v", index)

		blocks := intersectIndexBlocks(index.Blocks, indexFile.Range.Limit, recRange)
		if len(blocks) == 0 {
			// No block contains requested records. This could
			// happen because the BlockIndexEntry.Start of the first
			// block may not greater than index.Range.Start.
			continue
		}

		// Compute the approx # of bytes to read for sampledField.
		minFileOffset := blocks[0].FileOffset
		maxFileOffset := blocks[len(blocks)-1].FileOffset
		if minFileOffset > maxFileOffset {
			log.Panicf("corrupt offset: %d > %d", minFileOffset, maxFileOffset)
		}
		seqBytes := maxFileOffset - minFileOffset

		// Estimate the approx # of bytes to read across all fields.
		if sampledFieldSize <= 0 {
			// This shouldn't happen, given that we managed to read an nonempty index.
			return nil, fmt.Errorf("readandsubsetindexes %+v: seq file size is zero", indexFile)
		}
		rs := readSubshard{
			shardRange:  indexFile.Range,
			blocks:      blocks,
			approxBytes: int64(float64(seqBytes) * (float64(totalFileBytes) / float64(sampledFieldSize))),
		}
		indexes = append(indexes, rs)
	}
	return indexes, nil
}

// GenerateReadShardsOpts defines options to GenerateReadShards.
type GenerateReadShardsOpts struct {
	// Range defines an optional row shard range. Only records in this range will
	// be returned by Scan() and Read(). If Range is unset, the universal range is
	// assumed. See also ReadOpts.Range.
	Range biopb.CoordRange

	// SplitMappedCoords allows GenerateReadShards to split mapped reads of
	// the same <refid, alignment position> into multiple shards. Setting
	// this flag true will cause shard size to be more even, but the caller
	// must be able to handle split reads.
	SplitMappedCoords bool

	// SplitUnmappedCoords allows GenerateReadShards to split unmapped
	// reads into multiple shards. Setting this flag true will cause shard
	// size to be more even, but the caller must be able to handle split
	// unmapped reads.
	SplitUnmappedCoords bool

	// CombineMappedAndUnmappedCoords allows creating a shard that contains both
	// mapped and unmapped reads. If this flag is false, shards are always split
	// at the start of unmapped reads.
	AlwaysSplitMappedAndUnmappedCoords bool

	// BytesPerShard is the target shard size, in bytes across all fields.  If
	// this field is set, NumShards is ignored.
	BytesPerShard int64

	// NumShards specifies the number of shards to create. This field is ignored
	// if BytePerShard>0. If neither BytesPerShard nor NumShards is set,
	// runtime.NumCPU()*4 shards will be created.
	NumShards int
}

// GenerateReadShards returns a list of biopb.CoordRanges. The biopb.CoordRanges can be passed
// to NewReader for parallel, sharded record reads. The returned list satisfies
// the following conditions.
//
// 1. The ranges in the list fill opts.Range (or the UniversalRange if not set)
//    exactly, without an overlap or a gap.
//
// 2. Length of the list is at least nShards. The length may exceed nShards
//    because this function tries to split a range at a rowshard boundary.
//
// 3. The bytesize of the file region(s) that covers each biopb.CoordRange is roughly
// the same.
//
// 4. The ranges are sorted in an increasing order of biopb.Coord.
//
// If nShards specifies the number of shards. It should be generally be zero, in
// which case the function picks an appropriate default (currently, it uses
// runtime.NumCPU()*4)..
func GenerateReadShards(
	ctx context.Context,
	opts GenerateReadShardsOpts,
	path string,
	fields []string) ([]biopb.CoordRange, error) {
	if err := ValidateCoordRange(&opts.Range); err != nil {
		return nil, err
	}

	var indexFiles []FileInfo
	var err error
	if indexFiles, err = FindIndexFilesInRange(ctx, path, opts.Range); err != nil {
		return nil, err
	}
	var indexes []readSubshard
	if indexes, err = readAndSubsetIndexes(ctx, indexFiles, opts.Range, fields); err != nil {
		return nil, err
	}
	if len(indexes) == 0 {
		log.Printf("GenerateReadShards %s: no intersecting index found for %+v", path, opts.Range)
		// No data is found in the given range.
		return []biopb.CoordRange{opts.Range}, nil
	}

	totalBlocks := 0
	totalBytes := int64(0)
	for _, index := range indexes {
		totalBlocks += len(index.blocks)
		totalBytes += index.approxBytes
	}

	nShards := runtime.NumCPU() * 4
	if opts.BytesPerShard > 0 {
		nShards = int(totalBytes / opts.BytesPerShard)
	} else if opts.NumShards > 0 {
		nShards = opts.NumShards
	}
	log.Debug.Printf("GenerateReadShads %s: creating %d shards; totalblocks=%d, totalbytes=%d, opts %+v", path, nShards, totalBlocks, totalBytes, opts)
	targetBlocksPerReadShard := float64(totalBlocks) / float64(nShards)

	bounds := []biopb.CoordRange{}
	prevLimit := opts.Range.Start
	appendShard := func(limit biopb.Coord) { // Add shard [prevLimit, limit).
		cmp := limit.Compare(prevLimit)
		if cmp < 0 {
			log.Panicf("limit decreased %+v %+v", prevLimit, limit)
		}
		if cmp == 0 {
			return
		}
		if opts.AlwaysSplitMappedAndUnmappedCoords {
			unmappedStart := biopb.Coord{biopb.UnmappedRefID, 0, 0}
			if prevLimit.LT(unmappedStart) && limit.GT(unmappedStart) {
				bounds = append(bounds,
					biopb.CoordRange{prevLimit, unmappedStart},
					biopb.CoordRange{unmappedStart, limit})
				log.Debug.Printf("Add (%d): %+v", len(bounds), bounds[len(bounds)-1])
				prevLimit = limit
				return
			}
		}
		bounds = append(bounds, biopb.CoordRange{prevLimit, limit})
		log.Debug.Printf("Add (%d): %+v", len(bounds), bounds[len(bounds)-1])
		prevLimit = limit
	}

	nBlocks := 0
	for ii, index := range indexes {
		log.Debug.Printf("Index %d: range %+v bytes %+v ", ii, index.shardRange, index.approxBytes)
		for blockIndex, block := range index.blocks {
			if blockIndex > 0 && nBlocks > int(float64(len(bounds)+1)*targetBlocksPerReadShard) {
				// Add a shard boundary at block.StartAddr.
				limitAddr := block.StartAddr

				// Check if the end of the last block and start of this block share a
				// coordinate. This means the boundary is in the middle of a sequence of
				// reads at the coordinate. We can't split shards at such place, unless
				// opts.Split*Coords flags are set.
				prevBlock := index.blocks[blockIndex-1].EndAddr
				if prevBlock.RefId == limitAddr.RefId && prevBlock.Pos == limitAddr.Pos {
					if prevBlock.RefId != biopb.UnmappedRefID && !opts.SplitMappedCoords {
						log.Debug.Printf("prev (%d): %+v %+v, new: %+v", len(bounds), prevLimit, prevBlock, block.StartAddr)
						continue
					}
					if prevBlock.RefId == biopb.UnmappedRefID && !opts.SplitUnmappedCoords {
						log.Debug.Printf("prev (%d): %+v %+v, new: %+v", len(bounds), prevLimit, prevBlock, block.StartAddr)
						continue
					}
				}
				appendShard(limitAddr)
			}
			nBlocks++
		}
		// For performance, we don't want a readshard that crosses a rowshard
		// boundary, so close the shard here.
		log.Debug.Printf("Add (%d): %v", len(bounds), index.shardRange.Limit.Min(opts.Range.Limit))
		appendShard(index.shardRange.Limit.Min(opts.Range.Limit))
	}
	return bounds, nil
}
