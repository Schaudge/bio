package pamutil

import (
	"context"
	"fmt"

	"github.com/Schaudge/grailbase/errors"
	"github.com/Schaudge/grailbase/file"
	"github.com/Schaudge/grailbase/log"
	"github.com/Schaudge/grailbase/recordio"
	"github.com/Schaudge/grailbio/biopb"
	"github.com/Schaudge/hts/bam"
	"github.com/Schaudge/hts/sam"
)

// ReadShardIndex reads the index file, "dir/<recRange>.index".
func ReadShardIndex(ctx context.Context, dir string, recRange biopb.CoordRange) (index biopb.PAMShardIndex, err error) {
	path := ShardIndexPath(dir, recRange)

	in, err := file.Open(ctx, path)
	if err != nil {
		return index, errors.E(err, path)
	}
	defer file.CloseAndReport(ctx, in, &err)
	rio := recordio.NewScanner(in.Reader(ctx), recordio.ScannerOpts{})
	defer rio.Finish() // nolint: errcheck
	if !rio.Scan() {
		return index, errors.E(rio.Err(), fmt.Sprintf("readshardindex %v: %v", path, rio.Err()))
	}
	err = index.Unmarshal(rio.Get().([]byte))
	if err != nil {
		return index, err
	}
	if index.Magic != ShardIndexMagic {
		return index, fmt.Errorf("readshardindex %s: wrong index version '%v'; expect '%v'", dir, index.Magic, ShardIndexMagic)
	}
	if index.Version != DefaultVersion {
		return index, fmt.Errorf("readshardindex %s: wrong PAM version '%v'; expect '%v'", dir, index.Version, DefaultVersion)
	}
	return index, rio.Err()
}

// WriteShardIndex serializes "msg" into a single-block recordio file
// "dir/<coordRange>.index".  Existing contents of the file is clobbered.
func WriteShardIndex(ctx context.Context, dir string, coordRange biopb.CoordRange, msg *biopb.PAMShardIndex) error {
	path := ShardIndexPath(dir, coordRange)
	data, e := msg.Marshal()
	if e != nil {
		return e
	}
	out, e := file.Create(ctx, path)
	if e != nil {
		return e
	}
	err := errors.Once{}
	rio := recordio.NewWriter(out.Writer(ctx), recordio.WriterOpts{
		Transformers: []string{"zstd"},
	})
	rio.Append(data)
	err.Set(rio.Finish())
	err.Set(out.Close(ctx))
	return err.Err()
}

// FindIndexFilesInRange lists all *.index files that store a record that intersects "recRange".
func FindIndexFilesInRange(ctx context.Context, dir string, recRange biopb.CoordRange) ([]FileInfo, error) {
	var allFiles []FileInfo
	var err error
	if allFiles, err = ListIndexes(ctx, dir); err != nil {
		return nil, err
	}
	return ChooseIndexFilesInRange(allFiles, recRange)
}

// ChooseIndexFilesInRange returns the subset of allIndexFiles that overlap recRange.
// REQUIRES: allIndexFiles[i].Type == FileTypeShardIndex for all i.
func ChooseIndexFilesInRange(allIndexFiles []FileInfo, recRange biopb.CoordRange) ([]FileInfo, error) {
	var files []FileInfo
	for _, fi := range allIndexFiles {
		if fi.Type != FileTypeShardIndex {
			panic(fi)
		}
		if fi.Range.Intersects(recRange) {
			files = append(files, fi)
		}
	}
	return files, nil
}

// NewShardIndex creates a new PAMShardIndex object with the given arguments.
func NewShardIndex(shardRange biopb.CoordRange, h *sam.Header) biopb.PAMShardIndex {
	index := biopb.PAMShardIndex{}
	index.Magic = ShardIndexMagic
	index.Version = DefaultVersion
	var err error
	if index.EncodedBamHeader, err = bam.MarshalHeader(h); err != nil {
		// TODO(saito) propagate errors up
		log.Panicf("Encode header: %v", err)
	}
	index.Range = shardRange
	return index
}
