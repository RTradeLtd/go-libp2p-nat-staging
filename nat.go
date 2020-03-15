package nat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	nat "github.com/RTradeLtd/go-nat"
	"go.uber.org/zap"
)

var (
	// ErrNoMapping signals no mapping exists for an address
	ErrNoMapping = errors.New("mapping not established")
)

// DefaultTimeout for discovering nat
var DefaultTimeout = nat.DefaultTimeout

// MappingDuration is a default port mapping duration.
// Port mappings are renewed every (MappingDuration / 3)
const MappingDuration = time.Second * 60

// CacheTime is the time a mapping will cache an external address for
const CacheTime = time.Second * 15

// DiscoverNAT looks for a NAT device in the network and
// returns an object that can manage port mappings.
func DiscoverNAT(ctx context.Context, timeout time.Duration, logger *zap.Logger) (*NAT, error) {
	var (
		natInstance nat.NAT
		err         error
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// This will abort in 10 seconds anyways.
		natInstance, err = nat.DiscoverGateway(timeout)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err != nil {
		return nil, err
	}

	// Log the device addr.
	addr, err := natInstance.GetDeviceAddress()
	if err != nil {
		logger.Warn("DiscoverGateway error", zap.Error(err))
	} else {
		logger.Debug("gateway address found", zap.String("address", addr.String()))
	}

	return newNAT(ctx, natInstance, logger), nil
}

// NAT is an object that manages address port mappings in
// NATs (Network Address Translators). It is a long-running
// service that will periodically renew port mappings,
// and keep an up-to-date list of all the external addresses.
type NAT struct {
	natmu     sync.Mutex
	nat       nat.NAT
	ctx       context.Context
	cancel    context.CancelFunc
	mappingmu sync.RWMutex // guards mappings
	mappings  map[*mapping]struct{}
	logger    *zap.Logger
}

func newNAT(ctx context.Context, realNAT nat.NAT, logger *zap.Logger) *NAT {
	ctx, cancel := context.WithCancel(ctx)
	return &NAT{
		nat:      realNAT,
		ctx:      ctx,
		cancel:   cancel,
		mappings: make(map[*mapping]struct{}),
		logger:   logger.Named("libnat"),
	}
}

// Close shuts down all port mappings. NAT can no longer be used.
func (nat *NAT) Close() error {
	nat.cancel()
	return nil
}

// Mappings returns a slice of all NAT mappings
func (nat *NAT) Mappings() []Mapping {
	nat.mappingmu.Lock()
	maps2 := make([]Mapping, 0, len(nat.mappings))
	for m := range nat.mappings {
		maps2 = append(maps2, m)
	}
	nat.mappingmu.Unlock()
	return maps2
}

func (nat *NAT) addMapping(m *mapping) {
	nat.mappingmu.Lock()
	nat.mappings[m] = struct{}{}
	nat.mappingmu.Unlock()
}

func (nat *NAT) rmMapping(m *mapping) {
	nat.mappingmu.Lock()
	delete(nat.mappings, m)
	nat.mappingmu.Unlock()
}

// NewMapping attempts to construct a mapping on protocol and internal port
// It will also periodically renew the mapping until the returned Mapping
// -- or its parent NAT -- is Closed.
//
// May not succeed, and mappings may change over time;
// NAT devices may not respect our port requests, and even lie.
// Clients should not store the mapped results, but rather always
// poll our object for the latest mappings.
func (nat *NAT) NewMapping(protocol string, port int) (Mapping, error) {
	if nat == nil {
		return nil, fmt.Errorf("no nat available")
	}

	switch protocol {
	case "tcp", "udp":
	default:
		return nil, fmt.Errorf("invalid protocol: %s", protocol)
	}
	ctx, cancel := context.WithCancel(nat.ctx)
	m := &mapping{
		ctx:     ctx,
		cancel:  cancel,
		intport: port,
		nat:     nat,
		proto:   protocol,
	}

	nat.addMapping(m)
	go func() {
		ticker := time.NewTicker(MappingDuration / 3)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nat.establishMapping(m)
			}
		}
	}()
	// do it once synchronously, so first mapping is done right away, and before exiting,
	// allowing users -- in the optimistic case -- to use results right after.
	nat.establishMapping(m)
	return m, nil
}

func (nat *NAT) establishMapping(m *mapping) {
	oldport := m.ExternalPort()

	nat.logger.Debug("attempting port map creation", zap.String("protocol", m.Protocol()), zap.Int("internal.port", m.InternalPort()))
	comment := "libp2p"

	nat.natmu.Lock()
	newport, err := nat.nat.AddPortMapping(m.Protocol(), m.InternalPort(), comment, MappingDuration)
	if err != nil {
		// Some hardware does not support mappings with timeout, so try that
		newport, err = nat.nat.AddPortMapping(m.Protocol(), m.InternalPort(), comment, 0)
	}
	nat.natmu.Unlock()

	if err != nil || newport == 0 {
		m.setExternalPort(0) // clear mapping
		nat.logger.Error("failed to establish mapping", zap.Error(err))
		// we do not close if the mapping failed,
		// because it may work again next time.
		return
	}

	m.setExternalPort(newport)
	if oldport != 0 && newport != oldport {
		nat.logger.Warn("failed to renew port mapping", zap.Int("old.port", oldport), zap.Int("new.port", newport))
	}
}
