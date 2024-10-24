package proxy

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// cacheSize is initial size of addr and IP list cache map.
	cacheSize = 64
)

// defaultFreq is default frequency a resolver refreshes DNS cache.
var (
	defaultFreq          = 3 * time.Second
	defaultLookupTimeout = 10 * time.Second
)

// onRefreshed is called when DNS are refreshed.
var onRefreshed = func() {}

type DNSCache interface {
	LookupIP(ctx context.Context, addr string) ([]net.IP, error)
	Fetch(ctx context.Context, addr string) ([]net.IP, error)
	Refresh()
	Stop()
}

// Resolver is DNS cache resolver which cache DNS resolve results in memory.
type Resolver struct {
	lookupIPFn    func(ctx context.Context, host string) ([]net.IP, error)
	lookupTimeout time.Duration

	logger *logrus.Logger

	lock   sync.RWMutex
	cache  map[string][]net.IP
	closer func()
}

// NewDNSResolver initializes DNS cache resolver and starts auto refreshing in a new goroutine.
// To stop refreshing, call `Stop()` function.
func NewDNSResolver(freq time.Duration, lookupTimeout time.Duration, resolver *net.Resolver, logger *logrus.Logger) (DNSCache, error) {
	if freq <= 0 {
		freq = defaultFreq
	}

	if lookupTimeout <= 0 {
		lookupTimeout = defaultLookupTimeout
	}

	ticker := time.NewTicker(freq)
	ch := make(chan struct{})
	closer := func() {
		ticker.Stop()
		close(ch)
	}

	// copy handler function to avoid race
	onRefreshedFn := onRefreshed
	lookupIPFn := func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := resolver.LookupIPAddr(ctx, host)

		if err != nil {
			return nil, err
		}

		ips := make([]net.IP, len(addrs))
		for i, ia := range addrs {
			ips[i] = ia.IP
		}

		return ips, nil
	}

	r := &Resolver{
		lookupIPFn:    lookupIPFn,
		lookupTimeout: lookupTimeout,
		cache:         make(map[string][]net.IP, cacheSize),
		closer:        closer,
		logger:        logger,
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				r.Refresh()
				onRefreshedFn()
			case <-ch:
				return
			}
		}
	}()

	return r, nil
}

// LookupIP lookups IP list from DNS server then it saves result in the cache.
// If you want to get result from the cache use `Fetch` function.
func (r *Resolver) LookupIP(ctx context.Context, addr string) ([]net.IP, error) {
	ips, err := r.lookupIPFn(ctx, addr)
	if err != nil {
		return nil, err
	}

	r.lock.Lock()
	r.cache[addr] = ips
	r.lock.Unlock()
	return ips, nil
}

// Fetch fetches IP list from the cache. If IP list of the given addr is not in the cache,
// then it lookups from DNS server by `Lookup` function.
func (r *Resolver) Fetch(ctx context.Context, addr string) ([]net.IP, error) {
	r.lock.RLock()
	ips, ok := r.cache[addr]
	r.lock.RUnlock()
	if ok {
		return ips, nil
	}
	return r.LookupIP(ctx, addr)
}

// Refresh refreshes IP list cache.
func (r *Resolver) Refresh() {
	r.lock.RLock()
	addrs := make([]string, 0, len(r.cache))
	for addr := range r.cache {
		addrs = append(addrs, addr)
	}
	r.lock.RUnlock()

	for _, addr := range addrs {
		ctx, cancelF := context.WithTimeout(context.Background(), r.lookupTimeout)
		if _, err := r.LookupIP(ctx, addr); err != nil {
			r.logger.WithFields(logrus.Fields{
				"error": err,
				"addr":  addr,
			}).Error("failed to refresh DNS cache")
		}
		cancelF()
	}
}

// Stop stops auto refreshing.
func (r *Resolver) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closer != nil {
		r.closer()
		r.closer = nil
	}
}
