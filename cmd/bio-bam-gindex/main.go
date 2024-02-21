package main

// See doc.go for documentation
import (
	"flag"
	"io"
	"os"
	"runtime"

	"github.com/Schaudge/grailbase/grail"
	"github.com/Schaudge/grailbio/encoding/bam"
)

var (
	shardSize = flag.Int("shard-size", 64*1024, "Approximate bytes per interval in index")
)

func main() {
	shutdown := grail.Init()
	defer shutdown()

	r := io.Reader(os.Stdin)
	w := io.Writer(os.Stdout)

	if err := bam.WriteGIndex(w, r, *shardSize, runtime.NumCPU()); err != nil {
		panic(err.Error())
	}
}
