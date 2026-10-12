// Command testsrv runs the hostile test server as a standalone process.
//
// It exists so the benchmark harness (and a human with curl) can point at a
// long-lived server without writing Go code.
//
//	testsrv -addr 127.0.0.1:8099 -size 67108864 -per-conn-rate 1048576
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"vkmdownload/testsrv"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8099", "listen address")
		size        = flag.Int64("size", 64<<20, "resource size in bytes")
		seed        = flag.Int64("seed", 1, "content seed")
		perConnRate = flag.Int64("per-conn-rate", 0, "per-connection throttle in bytes/sec (0 = unlimited)")
		noRange     = flag.Bool("no-range", false, "advertise and honour no Range at all")
		fakeRange   = flag.Bool("fake-range", false, "advertise Accept-Ranges but always answer 200")
		ignoreIf    = flag.Bool("ignore-if-range", false, "ignore If-Range and answer 200")
		lieCL       = flag.Int64("lie-content-length", 0, "advertise this Content-Length while sending a different amount")
		dropAfter   = flag.Int64("drop-after", 0, "hard-close the connection after N body bytes")
		dropProb    = flag.Float64("drop-prob", 0, "randomly hard-close after each chunk with this probability")
		cookie      = flag.String("require-cookie", "", "require session=<value> or answer 403")
	)
	flag.Parse()

	cfg := testsrv.DefaultConfig(*size)
	cfg.Seed = *seed
	cfg.PerConnRate = *perConnRate
	cfg.LieContentLength = *lieCL
	cfg.DropAfterBytes = *dropAfter
	cfg.DropProbability = *dropProb
	cfg.RequireCookie = *cookie

	switch {
	case *noRange:
		cfg.AdvertiseRange = false
		cfg.HonorRange = false
	case *fakeRange:
		cfg.AdvertiseRange = true
		cfg.HonorRange = false
	case *ignoreIf:
		cfg.AdvertiseRange = true
		cfg.HonorRange = true
		cfg.IgnoreIfRange = true
	default:
		cfg.AdvertiseRange = true
		cfg.HonorRange = true
	}

	srv := testsrv.New(cfg)
	base, err := srv.Start(*addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	fmt.Printf("testsrv listening on %s\n", base)
	fmt.Printf("  GET %s/file\n", base)
	fmt.Printf("  size=%d seed=%d\n", cfg.Size, cfg.Seed)
	if cfg.PerConnRate > 0 {
		fmt.Printf("  per-connection throttle: %s/s\n", humanBytes(cfg.PerConnRate))
	}
	fmt.Printf("  honor-range=%v advertise-range=%v ignore-if-range=%v\n",
		cfg.HonorRange, cfg.AdvertiseRange, cfg.IgnoreIfRange)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	_ = srv.Close()
	time.Sleep(50 * time.Millisecond)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
