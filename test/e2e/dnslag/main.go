// dnslag is a DNS relay that answers slowly on purpose.
//
// A shutdown-grade link is not just low bandwidth: a query can take many
// seconds, some never answer, and the client's own deadline decides which to
// give up on. None of that reproduces on loopback, where everything is
// instant, so this sits between a client and a real resolver and adds the
// delay by hand.
//
//	sudo go run ./test/e2e/dnslag --min 1s --max 5s --drop 5
//	# serves :53 and forwards to the local server on :5300
//
// Each query is delayed independently, so queries sent together come back out
// of order — which is the condition a send window has to survive.
package main

import (
	"flag"
	"log"
	"math/rand"
	"net"
	"sync/atomic"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:53", "address to serve on")
	upstream := flag.String("upstream", "127.0.0.1:5300", "resolver to forward to")
	min := flag.Duration("min", time.Second, "shortest added delay")
	max := flag.Duration("max", 10*time.Second, "longest added delay")
	drop := flag.Int("drop", 5, "percent of queries to swallow entirely")
	quiet := flag.Bool("quiet", false, "only report the summary")
	flag.Parse()

	if *max < *min {
		log.Fatalf("--max (%s) is shorter than --min (%s)", *max, *min)
	}
	pc, err := net.ListenPacket("udp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer pc.Close()
	up, err := net.ResolveUDPAddr("udp", *upstream)
	if err != nil {
		log.Fatalf("upstream: %v", err)
	}
	log.Printf("dnslag: %s -> %s, delay %s..%s, dropping %d%%",
		*listen, *upstream, *min, *max, *drop)

	var sent, dropped atomic.Int64
	go func() {
		for range time.Tick(15 * time.Second) {
			log.Printf("dnslag: %d answered, %d dropped", sent.Load(), dropped.Load())
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		delay := *min
		if *max > *min {
			delay += time.Duration(rand.Int63n(int64(*max - *min)))
		}
		// A dropped query is how a real resolver fails: no answer at all, so
		// the client's deadline is the only thing that ends the wait.
		if *drop > 0 && rand.Intn(100) < *drop {
			dropped.Add(1)
			if !*quiet {
				log.Printf("dnslag: dropped a query (%d B)", len(query))
			}
			continue
		}
		go func() {
			time.Sleep(delay)
			reply, err := forward(query, up)
			if err != nil {
				if !*quiet {
					log.Printf("dnslag: upstream failed: %v", err)
				}
				return
			}
			if _, err := pc.WriteTo(reply, from); err == nil {
				sent.Add(1)
				if !*quiet {
					log.Printf("dnslag: answered after %s", delay.Round(time.Millisecond))
				}
			}
		}()
	}
}

// forward sends one query upstream and waits for its answer.
func forward(query []byte, up *net.UDPAddr) ([]byte, error) {
	conn, err := net.DialUDP("udp", nil, up)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
