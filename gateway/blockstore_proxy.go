package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/util"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const getBlockTimeout = time.Second * 60

type proxyBlockstore struct {
	httpClient *http.Client
	gatewayURL []string
	validate   bool
	rand       *rand.Rand
}

var _ blockstore.Blockstore = (*proxyBlockstore)(nil)

var _ CarFetcher = (*proxyBlockstore)(nil)

// NewProxyBlockstore creates a new [blockstore.Blockstore] that is backed by one
// or more gateways that follow the [Trustless Gateway] specification.
//
// [Trustless Gateway]: https://specs.ipfs.tech/http-gateways/trustless-gateway/
func NewProxyBlockstore(gatewayURL []string, cdns *CachedDNS) (blockstore.Blockstore, error) {
	if len(gatewayURL) == 0 {
		return nil, errors.New("missing gateway URLs to which to proxy")
	}

	s := rand.NewSource(time.Now().Unix())
	rand := rand.New(s)

	// Transport with increased defaults than [http.Transport] such that
	// retrieving multiple blocks from a single gateway concurrently is fast.
	transport := &http.Transport{
		MaxIdleConns:        1000,
		MaxConnsPerHost:     100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	if cdns != nil {
		transport.DialContext = cdns.DialContext
	}

	return &proxyBlockstore{
		gatewayURL: gatewayURL,
		httpClient: &http.Client{
			Timeout:   getBlockTimeout,
			Transport: otelhttp.NewTransport(transport),
		},
		// Enables block validation by default. Important since we are
		// proxying block requests to untrusted gateways.
		validate: true,
		rand:     rand,
	}, nil
}

func (ps *proxyBlockstore) fetch(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	urlStr := fmt.Sprintf("%s/ipfs/%s?format=raw", ps.getRandomGatewayURL(), c)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	log.Debugw("raw fetch", "url", req.URL)
	req.Header.Set("Accept", "application/vnd.ipld.raw")
	resp, err := ps.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http error from block gateway: %s", resp.Status)
	}

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if ps.validate {
		nc, err := c.Prefix().Sum(rb)
		if err != nil {
			return nil, blocks.ErrWrongHash
		}
		if !nc.Equals(c) {
			return nil, blocks.ErrWrongHash
		}
	}

	return blocks.NewBlockWithCid(rb, c)
}

func (ps *proxyBlockstore) Has(ctx context.Context, c cid.Cid) (bool, error) {
	blk, err := ps.fetch(ctx, c)
	if err != nil {
		return false, err
	}
	return blk != nil, nil
}

func (ps *proxyBlockstore) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	blk, err := ps.fetch(ctx, c)
	if err != nil {
		return nil, err
	}
	return blk, nil
}

func (ps *proxyBlockstore) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	blk, err := ps.fetch(ctx, c)
	if err != nil {
		return 0, err
	}
	return len(blk.RawData()), nil
}

func (ps *proxyBlockstore) HashOnRead(enabled bool) {
	ps.validate = enabled
}

func (c *proxyBlockstore) Put(context.Context, blocks.Block) error {
	return util.ErrNotImplemented
}

func (c *proxyBlockstore) PutMany(context.Context, []blocks.Block) error {
	return util.ErrNotImplemented
}

func (c *proxyBlockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	return nil, util.ErrNotImplemented
}

func (c *proxyBlockstore) DeleteBlock(context.Context, cid.Cid) error {
	return util.ErrNotImplemented
}

func (ps *proxyBlockstore) getRandomGatewayURL() string {
	return ps.gatewayURL[ps.rand.Intn(len(ps.gatewayURL))]
}

func (ps *proxyBlockstore) Fetch(ctx context.Context, path string, cb DataCallback) error {
	urlStr := fmt.Sprintf("%s%s", ps.getRandomGatewayURL(), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	log.Debugw("car fetch", "url", req.URL)
	req.Header.Set("Accept", "application/vnd.ipld.car;order=dfs;dups=y")
	resp, err := ps.httpClient.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		errData, err := io.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("could not read error message: %w", err)
		} else {
			err = fmt.Errorf("%q", string(errData))
		}
		return fmt.Errorf("http error from car gateway: %s: %w", resp.Status, err)
	}

	err = cb(path, resp.Body)
	if err != nil {
		resp.Body.Close()
		return err
	}
	return resp.Body.Close()
}
