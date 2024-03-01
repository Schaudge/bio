package bamprovider

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Schaudge/grailbase/errors"
	"github.com/Schaudge/grailbase/file"
	"github.com/Schaudge/grailbase/vcontext"
	"github.com/Schaudge/grailbio/biopb"
	gbam "github.com/Schaudge/grailbio/encoding/bam"
	"github.com/Schaudge/hts/bam"
	"github.com/Schaudge/hts/bgzf"
	"github.com/Schaudge/hts/bgzf/index"
	"github.com/Schaudge/hts/sam"
	"v.io/x/lib/vlog"
)

const (
	// DefaultBytesPerShard is the default value for GenerateShardsOpts.BytesPerShard
	DefaultBytesPerShard = int64(128 << 20)
	// DefaultMinBasesPerShard is the default value for GenerateShardsOpts.MinBasesPerShard
	DefaultMinBasesPerShard = 10000
)

// BAMProvider implements Provider for BAM files.  Both BAM and the index
// filenames are allowed to be S3 URLs, in which case the data will be read from
// S3. Otherwise the data will be read from the local filesystem.
type BAMProvider struct {
	// Path of the *.bam file. Must be nonempty.
	Path string
	// Index is the pathname of *.bam.bai file. If "", Path + ".bai"
	Index string
	err   errors.Once

	mu        sync.Mutex
	nActive   int
	freeIters []*bamIterator

	indexOnce sync.Once
	bindex    *bam.Index
	gindex    *gbam.GIndex

	infoOnce sync.Once
	header   *sam.Header
	info     FileInfo
}

type bamIterator struct {
	provider *BAMProvider
	in       file.File
	reader   *bam.Reader
	// Offset of the first record in the file.
	firstRecord bgzf.Offset
	// Half-open coordinate range to read.
	startAddr, limitAddr biopb.Coord

	active bool
	err    error
	next   *sam.Record
	done   bool
}

func (b *BAMProvider) indexPath() string {
	index := b.Index
	if index == "" {
		index = b.Path + ".bai"
		_, err := os.Stat(index)
		if err != nil {
			index = b.Path[:len(b.Path)-3] + ".bai"
		}
	}
	return index
}

// readIndex reads the *.bai file and caches its contents in b.index.  Repeated
// calls to this function returns b.index.
func (b *BAMProvider) readIndex() error {
	b.indexOnce.Do(func() {
		ctx := vcontext.Background()
		in, err := file.Open(ctx, b.indexPath())
		if err != nil {
			b.err.Set(err)
			return
		}
		var bindex *bam.Index
		var gindex *gbam.GIndex
		if strings.HasSuffix(b.indexPath(), ".gbai") {
			gindex, err = gbam.ReadGIndex(in.Reader(ctx))
		} else {
			bindex, err = bam.ReadIndex(in.Reader(ctx))
		}
		if err != nil {
			b.err.Set(err)
			return
		}
		if err = in.Close(ctx); err != nil {
			b.err.Set(err)
			return
		}
		b.bindex = bindex
		b.gindex = gindex
	})
	return b.err.Err()
}

// FileInfo implements the Provider interface.
func (b *BAMProvider) FileInfo() (FileInfo, error) {
	b.initInfo()
	if err := b.err.Err(); err != nil {
		return FileInfo{}, err
	}
	return b.info, nil
}

// GetHeader implements the Provider interface.
func (b *BAMProvider) GetHeader() (*sam.Header, error) {
	b.initInfo()
	if err := b.err.Err(); err != nil {
		return nil, err
	}
	return b.header, nil
}

// InitInfo sets b.info and b.header fields.
func (b *BAMProvider) initInfo() {
	b.infoOnce.Do(func() {
		ctx := vcontext.Background()
		reader, err := file.Open(ctx, b.Path)
		if err != nil {
			b.err.Set(err)
			return
		}
		info, err := reader.Stat(ctx)
		if err != nil {
			b.err.Set(err)
			reader.Close(ctx) // nolint: errcheck
			return
		}
		b.info = FileInfo{ModTime: info.ModTime(), Size: info.Size()}
		bamReader, err := bam.NewReader(reader.Reader(ctx), 1)
		if err != nil {
			b.err.Set(err)
			reader.Close(ctx) // nolint: errcheck
			return
		}
		b.header = bamReader.Header()
		if err := bamReader.Close(); err != nil {
			b.err.Set(err)
			reader.Close(ctx) // nolint: errcheck
			return
		}
		if err := reader.Close(ctx); err != nil {
			b.err.Set(err)
			return
		}
	})
}

// Close implements the Provider interface.
func (b *BAMProvider) Close() error {
	if b.nActive > 0 {
		vlog.Panicf("%d iterators still active for %+v", b.nActive, b)
	}
	for _, iter := range b.freeIters {
		iter.internalClose()
	}
	b.freeIters = nil
	return b.err.Err()
}

func (b *BAMProvider) freeIterator(i *bamIterator) {
	if !i.active {
		vlog.Panic(i)
	}
	i.active = false
	if i.Err() != nil {
		// The iter may be invalid. Don't reuse it.
		vlog.Errorf("freeiterator: %v", i.Err())
		i.internalClose() // Will set b.err
		i = nil
	}
	b.mu.Lock()
	if i != nil {
		b.freeIters = append(b.freeIters, i)
	}
	b.nActive--
	if b.nActive < 0 {
		vlog.Panicf("Negative active count for %+v", b)
	}
	b.mu.Unlock()
}

// Return an unused iterator. If b.freeIters is nonempty, this function returns
// one from freeIters. Else, it opens the BAM file, creates a BAM reader and
// returns an iterator containing them. On error, returns an iterator with
// non-nil err field.
func (b *BAMProvider) allocateIterator() *bamIterator {
	b.mu.Lock()
	b.nActive++
	if len(b.freeIters) > 0 {
		iter := b.freeIters[len(b.freeIters)-1]
		iter.active = true
		iter.err = nil
		iter.done = false
		iter.next = nil
		b.freeIters = b.freeIters[:len(b.freeIters)-1]
		b.mu.Unlock()
		return iter
	}
	b.mu.Unlock()

	iter := bamIterator{
		provider: b,
		active:   true,
	}
	if iter.err = b.readIndex(); iter.err != nil {
		return &iter
	}
	ctx := vcontext.Background()
	if iter.in, iter.err = file.Open(ctx, b.Path); iter.err != nil {
		return &iter
	}
	if iter.reader, iter.err = bam.NewReader(iter.in.Reader(ctx), 1); iter.err != nil {
		return &iter
	}
	iter.firstRecord = iter.reader.LastChunk().End
	return &iter
}

// GenerateShards implements the Provider interface.
func (b *BAMProvider) GenerateShards(opts GenerateShardsOpts) ([]gbam.Shard, error) {
	// Not strictly necessary (we don't attempt coordinate splitting for BAMs),
	// but it's best for this usage error to be independent of whether the file
	// is actually a BAM or PAM.
	// (could add this to fakeprovider too?)
	if (opts.SplitMappedCoords || opts.SplitUnmappedCoords) && (opts.Padding != 0) {
		return nil, fmt.Errorf("GenerateShards: nonzero Padding cannot be specified with Split*Coords")
	}

	header, err := b.GetHeader()
	if err != nil {
		return nil, err
	}
	if opts.BytesPerShard <= 0 {
		if opts.NumShards > 0 {
			info, err := file.Stat(vcontext.Background(), b.Path)
			if err != nil {
				return nil, err
			}
			opts.BytesPerShard = info.Size() / int64(opts.NumShards)
		}
		if opts.BytesPerShard < DefaultBytesPerShard {
			opts.BytesPerShard = DefaultBytesPerShard
		}
	}
	if opts.MinBasesPerShard <= 0 {
		opts.MinBasesPerShard = DefaultMinBasesPerShard
	}
	if opts.Strategy == ByteBased {
		return gbam.GetByteBasedShards(
			b.Path, b.indexPath(), opts.BytesPerShard, opts.MinBasesPerShard, opts.Padding, opts.IncludeUnmapped)
	}
	return gbam.GetPositionBasedShards(
		header, 100000, opts.Padding, opts.IncludeUnmapped)
}

// GetFileShards implements the Provider interface.
func (b *BAMProvider) GetFileShards() ([]gbam.Shard, error) {
	header, err := b.GetHeader()
	if err != nil {
		return nil, err
	}
	return []gbam.Shard{gbam.UniversalShard(header)}, nil
}

// NewIterator implements the Provider interface.
func (b *BAMProvider) NewIterator(shard gbam.Shard) Iterator {
	iter := b.allocateIterator()
	if iter.err != nil {
		return iter
	}
	iter.reset(shard.StartRef, shard.PaddedStart(), shard.EndRef, shard.PaddedEnd())
	return iter
}

// Reset the iterator to read the range [<startRef,startPos>, <endRef, endPos>).
func (i *bamIterator) reset(startRef *sam.Reference, startPos int, endRef *sam.Reference, endPos int) {
	header := i.reader.Header()
	i.startAddr = biopb.Coord{int32(startRef.ID()), int32(startPos), 0}
	i.limitAddr = biopb.Coord{int32(endRef.ID()), int32(endPos), 0}
	if i.startAddr.GE(i.limitAddr) {
		i.err = fmt.Errorf("start coord (%v) not before limit coord (%v)", i.startAddr, i.limitAddr)
		return
	}

	// Read the index and find the file offset at which <startRef,startPos> is
	// located.
	var (
		offset bgzf.Offset
		err    error
		ref    = startRef
	)
	for {
		var found bool
		if ref == nil {
			if i.provider.gindex != nil {
				offset = i.provider.gindex.UnmappedOffset()
			} else {
				offset, err = i.legacyFindUnmappedOffset()
			}
			break
		}
		start := 0
		if ref.ID() == startRef.ID() {
			start = startPos
		}
		end := ref.Len()
		if ref.ID() == endRef.ID() {
			end = endPos
		}
		if i.provider.gindex != nil {
			offset = i.provider.gindex.RecordOffset(int32(ref.ID()), int32(start), 0)
			found = true
		} else {
			found, offset, err = i.legacyFindRecordOffset(ref, start, end)
		}

		if err != nil || found {
			break
		}
		if ref.ID() == endRef.ID() {
			// No refs in range [startRef,endRef] has any index.  There's no record to
			// read.
			i.err = io.EOF
			return
		}
		// No index is found for this ref. Try the next ref.
		if ref.ID()+1 < len(header.Refs()) {
			ref = header.Refs()[ref.ID()+1]
		} else {
			ref = nil // unmapped section
		}
	}
	if err != nil {
		i.err = err
		return
	}
	i.err = i.reader.Seek(offset)
}

// Err implements the Iterator interface.
func (i *bamIterator) Err() error {
	if i.err == io.EOF {
		return nil
	}
	return i.err
}

// Close implements the Iterator interface.
func (i *bamIterator) Close() error {
	err := i.Err()
	i.provider.freeIterator(i)
	return err
}

// Find the the file offset at which the first unmapped sequence is
// stored. This function is conservative; it may return an offset that's smaller
// than absolutely necessary.
func (i *bamIterator) legacyFindUnmappedOffset() (bgzf.Offset, error) {
	// TODO(saito) cache the result.
	//
	// Iterate through the endpoint of each reference to find the
	// largest offset.
	header := i.reader.Header()
	var lastOffset bgzf.Offset
	foundRefs := false
	for _, r := range header.Refs() {
		chunks, err := i.provider.bindex.Chunks(r, 0, r.Len())
		if err == index.ErrInvalid {
			// There are no reads on this reference, but don't worry about it.
			continue
		}
		if err != nil {
			return lastOffset, err
		}
		foundRefs = true
		c := chunks[len(chunks)-1]
		if c.End.File > lastOffset.File ||
			(c.End.File == lastOffset.File && c.End.Block > lastOffset.Block) {
			lastOffset = c.End
		}
	}
	if !foundRefs {
		return i.firstRecord, nil
	}
	return lastOffset, nil
}

// Find the the file offset at which the first record at coordinate <ref,pos> is
// stored. This function is conservative; it may return an offset that's smaller
// than absolutely necessary.
func (i *bamIterator) legacyFindRecordOffset(ref *sam.Reference, startPos, endPos int) (bool, bgzf.Offset, error) {
	chunks, err := i.provider.bindex.Chunks(ref, startPos, endPos)
	if err == index.ErrInvalid || len(chunks) == 0 {
		// No reads for this interval: return an empty iterator.
		// This needs to be handled as a special case due to the current behavior of biogo.
		// Return the same 'eofIterator' to avoid unnecessary memory allocations, this
		// is likely an artifact of micro-benchmarking which uses smaller files which
		// are likely to hit this codepath.
		return false, bgzf.Offset{}, nil
	}
	if err != nil {
		return false, bgzf.Offset{}, err
	}
	return true, chunks[0].Begin, nil
}

func (i *bamIterator) Scan() bool {
	if !i.active {
		vlog.Panic("Reusing iterator")
	}
	if i.err != nil {
		return false
	}
	for {
		i.next, i.err = i.reader.Read()
		if i.err != nil {
			return false
		}
		recAddr := gbam.CoordFromSAMRecord(i.next, 0)
		if recAddr.LT(i.startAddr) {
			continue
		}
		return recAddr.LT(i.limitAddr)
	}
}

func (i *bamIterator) Record() *sam.Record {
	return i.next
}

func (i *bamIterator) internalClose() {
	if i.reader != nil {
		if err := i.reader.Close(); err != nil && i.err == nil {
			i.err = err
		}
		i.reader = nil
	}
	if i.in != nil {
		if err := i.in.Close(vcontext.Background()); err != nil && i.err == nil {
			i.err = err
		}
		i.in = nil
	}
	i.provider.err.Set(i.Err())
}
