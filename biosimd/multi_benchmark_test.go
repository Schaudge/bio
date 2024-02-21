// Copyright 2019 GRAIL, Inc.  All rights reserved.
// Use of this source code is governed by the Apache-2.0
// license that can be found in the LICENSE file.

package biosimd_test

import (
	"runtime"
	"testing"

	"github.com/Schaudge/grailbase/simd"
	"github.com/Schaudge/grailbase/traverse"
)

// Utility functions to assist with benchmarking of embarrassingly parallel
// jobs.

// This mostly duplicates code in base/simd.  We probably want to move it to a
// more central location.

type multiBenchFunc func(dst, src []byte, nIter int) int

type taggedMultiBenchFunc struct {
	f   multiBenchFunc
	tag string
}

type bytesInitFunc func(src []byte)

type multiBenchmarkOpts struct {
	dstInit bytesInitFunc
	srcInit bytesInitFunc
}

func multiBenchmark(bf multiBenchFunc, benchmarkSubtype string, nDstByte, nSrcByte, nJob int, b *testing.B, opts ...multiBenchmarkOpts) {
	// 'bf' is expected to execute the benchmarking target nIter times.
	//
	// Given that, for each of the 3 nCpu settings below, multiBenchmark launches
	// 'parallelism' goroutines, where each goroutine has nIter set to roughly
	// (nJob / nCpu), so that the total number of benchmark-target-function
	// invocations across all threads is nJob.  It is designed to measure how
	// effective traverse.Each-style parallelization is at reducing wall-clock
	// runtime.
	totalCpu := runtime.NumCPU()
	cases := []struct {
		nCpu    int
		descrip string
	}{
		{
			nCpu:    1,
			descrip: "1Cpu",
		},
		// 'Half' is often the saturation point, due to hyperthreading.
		{
			nCpu:    (totalCpu + 1) / 2,
			descrip: "HalfCpu",
		},
		{
			nCpu:    totalCpu,
			descrip: "AllCpu",
		},
	}
	var dstInit bytesInitFunc
	var srcInit bytesInitFunc
	if len(opts) >= 1 {
		dstInit = opts[0].dstInit
		srcInit = opts[0].srcInit
	}
	for _, c := range cases {
		success := b.Run(benchmarkSubtype+c.descrip, func(b *testing.B) {
			dsts := make([][]byte, c.nCpu)
			srcs := make([][]byte, c.nCpu)
			for i := 0; i < c.nCpu; i++ {
				// Add 63 to prevent false sharing.
				newArrDst := simd.MakeUnsafe(nDstByte + 63)
				newArrSrc := simd.MakeUnsafe(nSrcByte + 63)
				if i == 0 {
					if dstInit != nil {
						dstInit(newArrDst)
					}
					if srcInit != nil {
						srcInit(newArrSrc)
					} else {
						for j := 0; j < nSrcByte; j++ {
							newArrSrc[j] = byte(j * 3)
						}
					}
				} else {
					if dstInit != nil {
						copy(newArrDst[:nDstByte], dsts[0])
					}
					copy(newArrSrc[:nSrcByte], srcs[0])
				}
				dsts[i] = newArrDst[:nDstByte]
				srcs[i] = newArrSrc[:nSrcByte]
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// May want to replace this with something based on testing.B's
				// RunParallel method.  (Haven't done so yet since I don't see a clean
				// way to make that play well with per-core preallocated buffers.)
				_ = traverse.Each(c.nCpu, func(threadIdx int) error {
					nIter := (((threadIdx + 1) * nJob) / c.nCpu) - ((threadIdx * nJob) / c.nCpu)
					_ = bf(dsts[threadIdx], srcs[threadIdx], nIter)
					return nil
				})
			}
		})
		if !success {
			panic("benchmark failed")
		}
	}
}

func bytesInitMax15(src []byte) {
	for i := 0; i < len(src); i++ {
		src[i] = byte(i*3) & 15
	}
}
