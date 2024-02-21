package cmd

// The main AF4 binary.  See README.md for usage.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Schaudge/grailbase/compress"
	"github.com/Schaudge/grailbase/errors"
	"github.com/Schaudge/grailbase/file"
	"github.com/Schaudge/grailbase/grail"
	"github.com/Schaudge/grailbase/log"
	gunsafe "github.com/Schaudge/grailbase/unsafe"
	"github.com/Schaudge/grailbase/vcontext"
	"github.com/Schaudge/grailbio/encoding/fastq"
	"github.com/Schaudge/grailbio/fusion"
)

type memStats struct {
	mu sync.Mutex
	// Below are copies of runtime.MemStats
	alloc      uint64
	totalAlloc uint64
	sys        uint64
	heapSys    uint64
}

func (m *memStats) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("Alloc: %v TotalAlloc: %v, Sys: %v, HeapSys: %v",
		m.alloc, m.totalAlloc, m.sys, m.heapSys)
}

func (m *memStats) update() {
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	m.mu.Lock()

	if m.alloc < s.Alloc {
		m.alloc = s.Alloc
	}
	if m.totalAlloc < s.TotalAlloc {
		m.totalAlloc = s.TotalAlloc
	}
	if m.sys < s.Sys {
		m.sys = s.Sys
	}
	if m.heapSys < s.HeapSys {
		m.heapSys = s.HeapSys
	}
	m.mu.Unlock()
}

// Collection of options set via cmdline flags
type fusionFlags struct {
	transcriptPath     string
	cosmicFusionPath   string
	r1, r2             string
	fastaOutputPath    string
	rioOutputPath      string
	rioInputPath       string
	filteredOutputPath string
	geneListInputPath  string
	geneListOutputPath string
}

func writeFASTA(out io.Writer, c fusion.Candidate, geneDB *fusion.GeneDB, opts fusion.Opts) {
	writeString := func(strings ...string) {
		for _, s := range strings {
			if _, err := out.Write(gunsafe.StringToBytes(s)); err != nil {
				log.Panic(err)
			}
		}
	}
	writeGeneLocation := func(gid fusion.GeneID) {
		gi := geneDB.GeneInfo(gid)
		if _, err := fmt.Fprintf(out, "%s:%d-%d:%d", gi.Chrom, gi.Start, gi.End, gi.Index); err != nil {
			log.Panic(err)
		}
	}
	writeReadRange := func(r fusion.CrossReadPosRange) {
		start := r.Start
		if r.Start.ReadType() == fusion.R2 {
			start = fusion.Pos(start.R2Off() + len(c.Frag.R1Seq) + 1)
		}
		end := r.End
		if end.ReadType() == fusion.R2 {
			end = fusion.Pos(end.R2Off() + len(c.Frag.R1Seq) + 1)
		}
		if _, err := fmt.Fprintf(out, "%d:%d", start, end-1 /*need a closed range*/); err != nil {
			log.Panic(err)
		}
	}

	geneOrder := fusion.CosmicOrder
	if opts.Denovo {
		geneOrder = fusion.AlphabeticalOrder
	}
	writeString(">", c.Frag.Name, "|")
	// Emit gene names.
	for i, fi := range c.Fusions {
		if i > 0 {
			writeString(",")
		}
		g1, g2 := fusion.SortGenePair(geneDB, fi.G1ID, fi.G2ID, geneOrder)
		writeString(geneDB.GeneInfo(g1).Gene)
		writeString("/")
		writeString(geneDB.GeneInfo(g2).Gene)
	}
	writeString("|")
	// Emit gene locations
	for i, fi := range c.Fusions {
		if i > 0 {
			writeString(",")
		}
		g1, g2 := fusion.SortGenePair(geneDB, fi.G1ID, fi.G2ID, geneOrder)
		writeGeneLocation(g1)
		writeString("/")
		writeGeneLocation(g2)
	}
	writeString("|")
	if _, err := fmt.Fprintf(out, "%d/%d|", c.Fusions[0].G1Span, c.Fusions[0].G2Span); err != nil {
		panic(err.Error())
	}
	writeReadRange(c.Fusions[0].G1Range)
	writeString("/")
	writeReadRange(c.Fusions[0].G2Range)
	writeString("\n")
	writeString(c.Frag.R1Seq)
	if c.Frag.R2Seq != "" {
		writeString("|", c.Frag.R2Seq)
	}
	writeString("\n")
}

// A uint64 sequence number defines a total ordering of reads from multiple
// fastq files. The final list of fusion reads is sorted in order of appearance
// in the fastq files.  The sequence is a combination of <file index, read index
// within the file>.
func newSeq(fileseq, readseq uint) uint64 {
	return (uint64(fileseq) << 48) | uint64(readseq)
}

const invalidSeq = math.MaxUint64

type req struct {
	seq                uint64
	name, r1Seq, r2Seq string
}

type res struct {
	seq       uint64
	candidate fusion.Candidate

	// stats is sent as the very last record, with seq=invalidSeq.
	stats fusion.Stats
}

func processRequests(reqCh chan req, resCh chan res, geneDB *fusion.GeneDB, opts fusion.Opts) {
	stitcher := fusion.NewStitcher(opts.KmerLength, opts.LowComplexityFraction)
	stats := fusion.Stats{}
	for req := range reqCh {
		// TODO(saito,xyang) UMI removal should be done when reading the files, not
		// here.
		stats.Fragments++
		name, r1Seq, r2Seq := fusion.MaybeRemoveUMI(req.name, req.r1Seq, req.r2Seq, opts)
		r1Seq, r2Seq = fusion.RemoveLowComplexityReads(r1Seq, r2Seq, &stats, opts)
		frag := stitcher.Stitch(name, r1Seq, r2Seq, &stats)
		fusions := fusion.DetectFusion(geneDB, frag, &stats, opts)
		if len(fusions) == 0 {
			stitcher.FreeFragment(frag)
			continue
		}
		resCh <- res{seq: req.seq, candidate: fusion.Candidate{frag, fusions}}
	}
	resCh <- res{seq: invalidSeq, stats: stats}
}

func readFASTQ(ctx context.Context, reqCh chan req, fileseq uint, r1Path, r2Path string) {
	var (
		sc       *fastq.PairScanner
		r1R, r2R fastq.Read
		nRead    uint
	)

	openFASTQ := func(path string) (file.File, io.ReadCloser) {
		in, err := file.Open(ctx, path)
		if err != nil {
			log.Panicf("open %v: %v", path, err)
		}
		gz, _ := compress.NewReader(in.Reader(ctx))
		return in, gz
	}
	closeFASTQ := func(in file.File, r io.ReadCloser, path string) {
		if err := r.Close(); err != nil {
			log.Panicf("close gzip %s: %v", path, err)
		}
		if err := in.Close(ctx); err != nil {
			log.Panicf("close %s: %v", path, err)
		}
	}

	in1, inr1 := openFASTQ(r1Path)
	in2, inr2 := openFASTQ(r2Path)
	sc = fastq.NewPairScanner(inr1, inr2, fastq.ID|fastq.Seq)
	for {
		if !sc.Scan(&r1R, &r2R) {
			break
		}
		nRead++
		if nRead%(1024*1024) == 0 {
			log.Printf("%s: %dMi readpairs", r1Path, nRead/(1024*1024))
		}
		id := r1R.ID
		if len(id) == 0 || id[0] != '@' {
			log.Panicf("Corrupt fastq record: %+v", r1R)
		}
		id = id[1:]
		reqCh <- req{newSeq(fileseq, nRead), id, r1R.Seq, r2R.Seq}
	}
	log.Printf("Processed %d reads in %s", nRead, r1Path)
	if err := sc.Err(); err != nil {
		log.Panicf("close pair: %v", err)
	}
	closeFASTQ(in1, inr1, r1Path)
	closeFASTQ(in2, inr2, r2Path)
}

func processFASTQ(ctx context.Context, fileseq uint,
	r1Path, r2Path string,
	geneDB *fusion.GeneDB, opts fusion.Opts) ([]res, fusion.Stats) {
	reqCh := make(chan req, 1024*64)
	resCh := make(chan res, 1024)

	wg1 := sync.WaitGroup{}
	parallelism := runtime.NumCPU()
	for i := 0; i < parallelism; i++ {
		wg1.Add(1)
		go func() {
			processRequests(reqCh, resCh, geneDB, opts)
			wg1.Done()
		}()
	}

	wg2 := sync.WaitGroup{}
	wg2.Add(1)
	var (
		results []res
		stats   fusion.Stats
	)
	go func() {
		for res := range resCh {
			if res.seq == invalidSeq {
				stats = stats.Merge(res.stats)
				continue
			}
			results = append(results, res)
		}
		wg2.Done()
	}()

	readFASTQ(ctx, reqCh, fileseq, r1Path, r2Path)
	close(reqCh)
	wg1.Wait()
	close(resCh)
	wg2.Wait()
	return results, stats
}

// writeGeneList dumps names of all the genes registered in geneDB.
func writeGeneList(ctx context.Context, geneListOutputPath string, geneDB *fusion.GeneDB) {
	out, err := file.Create(ctx, geneListOutputPath)
	if err != nil {
		log.Panic(err)
	}
	w := bufio.NewWriter(out.Writer(ctx))
	min, limit := geneDB.GeneIDRange()
	er := errors.Once{}
	n := 0
	for id := min; id < limit; id++ {
		gene := geneDB.GeneInfo(id)
		_, err := w.WriteString(gene.Gene)
		er.Set(err)
		er.Set(w.WriteByte('\n'))
		n++
	}
	er.Set(w.Flush())
	er.Set(out.Close(ctx))
	if er.Err() != nil {
		log.Panic(err)
	}
	log.Printf("Wrote %d genes to %s", n, geneListOutputPath)
}

// readGeneList reads a file produced by writeGeneList and prepopulates the
// geneDB. It can be used to control genename -> geneID mappings. GeneIDs are
// assigned in FCFS order in the genelist file.
func readGeneList(ctx context.Context, geneDB *fusion.GeneDB, geneListInputPath string) {
	data, err := file.ReadFile(ctx, geneListInputPath)
	if err != nil {
		log.Panic(err)
	}
	var genes []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			genes = append(genes, line)
		}
	}
	geneDB.PrepopulateGenes(genes)
	log.Printf("Interned %d genes from %s", len(genes), geneListInputPath)
}

func generateCandidates(
	ctx context.Context,
	r1Paths, r2Paths []string,
	geneListInputPath string,
	geneListOutputPath string,
	cosmicFusionPath string,
	transcriptomePath string,
	opts fusion.Opts) (*fusion.GeneDB, []fusion.Candidate) {
	geneDB := fusion.NewGeneDB(opts)

	log.Printf("Start reading geneDB")
	if geneListInputPath != "" {
		readGeneList(ctx, geneDB, geneListInputPath)
	}
	if cosmicFusionPath != "" {
		geneDB.ReadFusionEvents(ctx, cosmicFusionPath)
	}
	geneDB.ReadTranscriptome(ctx, transcriptomePath, cosmicFusionPath != "")
	if geneListOutputPath != "" {
		writeGeneList(ctx, geneListOutputPath, geneDB)
		log.Printf("Exiting early because --gene-list-output is s et")
		os.Exit(0)
	}
	log.Printf("Start reading fastq")
	var (
		allResultsMu sync.Mutex
		allResults   []res
		allStats     fusion.Stats
		wg           sync.WaitGroup
	)
	for i := range r1Paths {
		wg.Add(1)
		go func(i int) {
			c, stats := processFASTQ(ctx, uint(i), r1Paths[i], r2Paths[i], geneDB, opts)
			allResultsMu.Lock()
			allResults = append(allResults, c...)
			allStats = allStats.Merge(stats)
			allResultsMu.Unlock()
			wg.Done()
		}(i)
	}
	wg.Wait()
	sort.SliceStable(allResults, func(i, j int) bool {
		return allResults[i].seq < allResults[j].seq
	})
	allCandidates := make([]fusion.Candidate, len(allResults))
	for i := range allResults {
		allCandidates[i] = allResults[i].candidate
	}
	log.Printf("Stats: Finished stage1: %+v", allStats)
	return geneDB, allCandidates
}

func filterCandidates(
	ctx context.Context,
	allCandidates []fusion.Candidate, geneDB *fusion.GeneDB, opts fusion.Opts) []fusion.Candidate {
	var (
		filteredCandidates                            []fusion.Candidate
		nSkippedLowComplexity, nSkippedCloseProximity int
	)
	for _, c := range allCandidates {
		var k int
		// Remove bad fusion events
		for _, fi := range c.Fusions {
			if fusion.LinkedByLowComplexSubstring(c.Frag, fi, opts.LowComplexityFraction) {
				nSkippedLowComplexity++
				continue
			}
			// Note: we want to keep genes in proximity to distinguish overlapping
			// genes and read-through events.
			if fusion.CloseProximity(geneDB, fi, opts.MaxProximityDistance, opts.MaxProximityGenes) {
				nSkippedCloseProximity++
				continue
			}
			c.Fusions[k] = fi
			k++
		}
		c.Fusions = c.Fusions[:k]

		// Drop the candidate fragment if there's no fusion event left.
		if len(c.Fusions) > 0 {
			filteredCandidates = append(filteredCandidates, c)
		}
	}
	log.Printf("Stats: %d of %d remaining after removing %d low-complex substring and %d close proximity", len(filteredCandidates), len(allCandidates),
		nSkippedLowComplexity, nSkippedCloseProximity)

	fusion.FilterDuplicates(&filteredCandidates, opts.UMIInName)
	log.Printf("Stats: %d remaining after removing duplicates", len(filteredCandidates))
	fusion.FilterByMinSpan(opts.UMIInName, opts.MinSpan, &filteredCandidates, opts.MinReadSupport)
	log.Printf("Stats: %d remaining after filtering by minspan", len(filteredCandidates))
	fusion.DiscardAbundantPartners(&filteredCandidates, opts.MaxGenePartners)
	log.Printf("Stats: %d remaining after removing genes with abundant partners", len(filteredCandidates))
	return filteredCandidates
}

// DetectFusion is the main entry point for AF4 fusion detector.
func DetectFusion(ctx context.Context, flags fusionFlags, opts fusion.Opts) {
	var (
		geneDB        *fusion.GeneDB
		allCandidates []fusion.Candidate
	)
	if flags.rioInputPath == "" {
		// Generate candidates from scratch
		opts.Denovo = (flags.cosmicFusionPath == "")
		r1Paths := strings.Split(flags.r1, ",")
		r2Paths := strings.Split(flags.r2, ",")
		if len(r1Paths) != len(r2Paths) {
			log.Panicf("There must be the same # of R1 and R2 files: '%s' <-> '%s'", flags.r1, flags.r2)
		}
		geneDB, allCandidates = generateCandidates(ctx, r1Paths, r2Paths,
			flags.geneListInputPath, flags.geneListOutputPath,
			flags.cosmicFusionPath,
			flags.transcriptPath, opts)
		fastaOut, cleanup1 := createFile(ctx, flags.fastaOutputPath)
		var rioOut *fusionWriter
		if flags.rioOutputPath != "" {
			rioOut = newFusionWriter(ctx, flags.rioOutputPath, geneDB, opts)
		}
		for _, c := range allCandidates {
			writeFASTA(fastaOut, c, geneDB, opts)
			if rioOut != nil {
				rioOut.Write(c)
			}
		}
		cleanup1()
		if rioOut != nil {
			rioOut.Close(ctx)
		}
	} else {
		// Read candidates, genedb, and options from a recordio dump.
		r := newFusionReader(ctx, flags.rioInputPath)
		for r.Scan() {
			allCandidates = append(allCandidates, r.Get())
		}
		geneDB = r.GeneDB()
		opts = r.Opts()
		r.Close(ctx)
	}
	log.Printf("Stats: %d candidates after stage 1", len(allCandidates))
	filteredCandidates := filterCandidates(ctx, allCandidates, geneDB, opts)
	filteredOut, cleanup2 := createFile(ctx, flags.filteredOutputPath)
	for _, c := range filteredCandidates {
		writeFASTA(filteredOut, c, geneDB, opts)
	}
	cleanup2()
	log.Printf("Stats: %d final candidates", len(filteredCandidates))
}

// Usage prints the command usage and exits the process.  It can be used as a
// flag.Usage callback.
func usage() {
	// TODO(saito) This doc is only for gencode. Update once we have a full README.
	fmt.Fprintln(os.Stderr, `
See [README.md](https://github.com/Schaudge/grailbio/fusion/README.md) for more details.`)
	flag.PrintDefaults()
	os.Exit(1)
}

func Run() {
	flag.Usage = usage

	// Flags for gencode->fasta translator.
	generateTranscriptomeFlag := false
	gencodeFlags := gencodeFlags{}
	flag.BoolVar(&generateTranscriptomeFlag, "generate-transcriptome", false, "Generate a transcriptome FASTA file.")
	flag.IntVar(&gencodeFlags.exonPadding, "exon-padding", 0, "Residues to pad exons by. (default 0, minimum 0)")
	flag.StringVar(&gencodeFlags.output, "output", "", "Path to an output file. (default stdout)")
	flag.BoolVar(&gencodeFlags.codingOnly, "coding-only", false, "Output protein coding transcripts only.")
	flag.BoolVar(&gencodeFlags.separateJns, "separate-junctions", false, `Print the regular transcript and then add the junctions to the
end of the sequence (separated by |'s. This is recommended if
using the output fasta for fusion calling.`)
	flag.IntVar(&gencodeFlags.retainedExonBases, "retained-exon-bases", 18,
		`If -separate_junctions is specified, how much of the exon should be retained? (default 18, minimum 1)`)
	flag.BoolVar(&gencodeFlags.wholeGenes, "whole-genes", false, `Print out the gene records from start to end instead of
printing individual transcripts. (-exon_padding will pad
genes)`)
	flag.BoolVar(&gencodeFlags.collapseTranscripts, "collapse-transcripts", false,
		`Print out all overlapping exonic regions (+<exon_padding>) identified for every gene (separated by |'s')`)
	flag.BoolVar(&gencodeFlags.keepMitochondrialGenes, "keep-mitochondrial-genes", false, "keep mitochondrial genes")
	flag.BoolVar(&gencodeFlags.keepReadthroughTranscripts, "keep-readthrough-transcripts", false,
		"Keep readthrough transcripts")
	flag.BoolVar(&gencodeFlags.keepPARYLocusTranscripts, "keep-pary-locus-transcripts", false,
		"Keep PARY locas transcripts")
	flag.BoolVar(&gencodeFlags.keepVersionedGenes, "keep-versioned-genes", false,
		"Keep versioned genes")
	// Flags for the fusion detector.
	opts := fusion.DefaultOpts
	fusionFlags := fusionFlags{}
	flag.StringVar(&fusionFlags.transcriptPath, "transcript", "", "File containing all transcripts")
	flag.StringVar(&fusionFlags.cosmicFusionPath, "cosmic-fusion", "", `Fixed list of fusions to query within the input.
If this flag is empty, all possible combinations of genes in the --transcript file will be examined as fusion candidates.`)
	flag.StringVar(&fusionFlags.r1, "r1", "", "Comma-separated list of Gzipped FASTQ files containing R1 reads.")
	flag.StringVar(&fusionFlags.r2, "r2", "", "Comma-separated list of Gzipped FASTQ files containing R2 reads.")
	flag.StringVar(&fusionFlags.fastaOutputPath, "fasta-output", "./all-outputs.fa", "FASTA file to store all candidates.")
	flag.StringVar(&fusionFlags.rioInputPath, "rio-input", "", "FASTA file that store all candidates. If this flag is nonempty, af4 will run only the 2nd filtering stage using the input. If this flag is empty (default) af4 will run the whole process from scratch.")
	flag.StringVar(&fusionFlags.rioOutputPath, "rio-output", "", "Recordio checkpoint file to store all candidates. If empty, the file will not be created")
	flag.StringVar(&fusionFlags.filteredOutputPath, "filtered-output", "./filtered-outputs.fa", "FASTA file to store all candidates.")
	flag.StringVar(&fusionFlags.geneListInputPath, "gene-list", "", `NOT FOR GENERAL USE. If set,
gene DB is seeded with the genes in this list. Gene IDs are assigned in
first-come, first-serve order, so this file can be used to explicitly assign
gene IDs to genes to maintain compatibility with old code`)
	flag.StringVar(&fusionFlags.geneListOutputPath, "gene-list-output", "", "NOT FOR GENERAL USE. If set, list of registered genes are written to this file")

	flag.BoolVar(&opts.UMIInRead, "umi-in-read", fusion.DefaultOpts.UMIInRead, "If true, UMI is embedded in the sequence.")
	flag.BoolVar(&opts.UMIInName, "umi-in-name", fusion.DefaultOpts.UMIInName, "If true, UMI is embedded in the readname.")
	flag.IntVar(&opts.KmerLength, "k", fusion.DefaultOpts.KmerLength, "Length of kmers")
	flag.IntVar(&opts.MaxGenesPerKmer, "max-genes-per-kmer", fusion.DefaultOpts.MaxGenesPerKmer, "Upper limit on the max number of genes that a kmer belongs to")
	flag.IntVar(&opts.MaxGenePartners, "max-gene-partners", fusion.DefaultOpts.MaxGenePartners, "The maximum number of partners a gene can have. Used in the 2nd stage only")
	flag.IntVar(&opts.MaxProximityDistance, "max-proximity-distance", fusion.DefaultOpts.MaxProximityDistance,
		`Upper limit on the distance cutoff below which a candidate will be rejected as a readthrough event.`)
	flag.IntVar(&opts.MaxProximityGenes, "max-proximity-genes", fusion.DefaultOpts.MaxProximityGenes,
		`Upper limit on the number of genes separating a gene pair (If on the same chromsosome) below which they will be flagged as read-through events`)
	flag.IntVar(&opts.MinSpan, "min-span", fusion.DefaultOpts.MinSpan, "min base evidence for a gene in the fusion")
	flag.IntVar(&opts.MaxHomology, "max-homology", fusion.DefaultOpts.MaxHomology, "max overlap allowed b/w genes in a fusion")

	cleanup := grail.Init()
	defer cleanup()
	ctx := vcontext.Background()
	var memStats memStats
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			memStats.update()
		}
	}()

	if generateTranscriptomeFlag {
		if flag.NArg() < 2 {
			log.Fatal("exactly two arguments (<gencode_gtf> <gencode_fasta>) are required")
		}
		gtfPath, fastaPath := flag.Arg(0), flag.Arg(1)
		GenerateTranscriptome(ctx, gtfPath, fastaPath, gencodeFlags)
	} else {
		DetectFusion(ctx, fusionFlags, opts)
	}
	memStats.update()
	log.Printf("MemStats: %s", memStats.String())
	log.Printf("All done")
}
