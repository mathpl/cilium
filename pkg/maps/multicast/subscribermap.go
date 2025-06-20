// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package multicast

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"unsafe"

	ciliumebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/hive/cell"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/datapath/linux/config/defines"
	"github.com/cilium/cilium/pkg/datapath/linux/probes"
	"github.com/cilium/cilium/pkg/ebpf"
)

// compile time checks
var _ GroupV4Map = (*GroupV4OuterMap)(nil)
var _ SubscriberV4Map = (*SubscriberV4InnerMap)(nil)

const (
	// Pinned outer map name which signals the existence of a multicast group
	// in the control plane.
	GroupOuter4MapName = "cilium_mcast_group_outer_v4_map"
	// Defines total number of multicast groups on a single node.
	MaxGroups = 1024
	// Defines total number of subscribers per multicast group on a single node.
	MaxSubscribers = 1024
)

// GroupV4Map provides an interface between the control and data plane,
// enabling the creation, deletion, and querying of IPv4 multicast groups
// and subscribers.
type GroupV4Map interface {
	Lookup(multicastAddr netip.Addr) (SubscriberV4Map, error)
	Insert(multicastAddr netip.Addr) error
	Delete(multicastAddr netip.Addr) error
	List() ([]netip.Addr, error)
}

// GroupV4OuterMap outer map keyed by GroupV4Key multicast group
// addresses.
type GroupV4OuterMap struct {
	*ebpf.Map

	// batchLookupSupported indicates if the kernel supports batch lookup.
	batchLookupSupported bool
	logger               *slog.Logger
}

func NewGroupV4OuterMap(logger *slog.Logger, name string) *GroupV4OuterMap {
	innerMap := newSubscriberV4InnerMapSpec()
	m := ebpf.NewMap(logger, &ebpf.MapSpec{
		Name:       name,
		Type:       ebpf.HashOfMaps,
		KeySize:    uint32(unsafe.Sizeof(GroupV4Key{})),
		ValueSize:  uint32(unsafe.Sizeof(GroupV4Val{})),
		MaxEntries: uint32(MaxGroups),
		InnerMap:   innerMap,
		Pinning:    ebpf.PinByName,
	})

	return &GroupV4OuterMap{logger: logger, Map: m}
}

// ParamsIn are parameters provided by the Hive and is the argument for
// NewGroupV4Map constructor
type ParamsIn struct {
	cell.In
	Lifecycle cell.Lifecycle
	Logger    *slog.Logger
	Config
}

// ParamsOut are the parameters provided to the Hive and is the return
// argument for NewGroupV4Map
type ParamsOut struct {
	cell.Out
	bpf.MapOut[GroupV4Map]
	defines.NodeOut
}

// NewGroupV4Map creates a new GroupV4Map
// and provides it to the hive dependency injection graph.
//
// Other subsystems can depend on the "multicast.GroupV4Map" type to obtain
// a handle to the datapath interface.
func NewGroupV4Map(in ParamsIn) ParamsOut {
	out := ParamsOut{}

	if !in.MulticastEnabled {
		return out
	}

	// must have "bpf_map_for_each_elem" helper available, if not, don't
	// initialize the map, dependent code should be checking if their map
	// dependency is nil or not.
	if probes.HaveProgramHelper(in.Logger, ciliumebpf.SchedCLS, asm.FnForEachMapElem) != nil {
		in.Logger.Error("Disabled support for BPF Multicast due to missing kernel support (Linux 5.13 or later)")
		return out
	}

	out.NodeDefines["ENABLE_MULTICAST"] = "1"

	groupMap := NewGroupV4OuterMap(in.Logger, GroupOuter4MapName)

	out.MapOut = bpf.NewMapOut((GroupV4Map(groupMap)))

	in.Lifecycle.Append(cell.Hook{
		OnStart: func(cell.HookContext) error {
			err := groupMap.OpenOrCreate()
			if err != nil {
				return err
			}
			groupMap.batchLookupSupported = haveBatchLookupSupport[GroupV4Key, GroupV4Val](groupMap.Map)
			return nil
		},
		OnStop: func(cell.HookContext) error {
			return groupMap.Close()
		},
	})

	return out
}

func (m GroupV4OuterMap) Insert(group netip.Addr) error {
	key, err := NewGroupV4KeyFromNetIPAddr(group)
	if err != nil {
		return err
	}

	subMap, err := newSubscriberV4InnerMap(m.logger)
	if err != nil {
		return fmt.Errorf("failed to create SubscriberV4InnerMap: %w", err)
	}

	val := GroupV4Val{
		FD: uint32(subMap.FD()),
	}

	err = m.Update(key, val, ciliumebpf.UpdateNoExist)
	if err != nil {
		subMap.Close()
		return fmt.Errorf("failed to create new multicast group entry: %w", err)
	}

	return nil
}

func (m GroupV4OuterMap) Lookup(group netip.Addr) (SubscriberV4Map, error) {
	var val GroupV4Val

	key, err := NewGroupV4KeyFromNetIPAddr(group)
	if err != nil {
		return nil, err
	}

	err = m.Map.Lookup(key.Group, &val)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil, fmt.Errorf("multicast group %s does not exist: %w", group.String(), err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query for multicast group: %w", err)
	}

	var subMap *ebpf.Map
	subMap, err = ebpf.MapFromID(m.logger, int(val.FD))
	if err != nil {
		return nil, fmt.Errorf("failed to convert SubscriberV4InnerMap FD to *ebpf.Map: %w", err)
	}

	return &SubscriberV4InnerMap{subMap}, nil
}

func (m GroupV4OuterMap) Delete(group netip.Addr) error {
	key, err := NewGroupV4KeyFromNetIPAddr(group)
	if err != nil {
		return err
	}
	return m.Map.Delete(key)
}

// List returns a list of all multicast groups in the map. Batch lookup is used to get the groups if supported.
// Batch lookup is supported in kernel version 5.19 and later for map.HashOfMaps
func (m GroupV4OuterMap) List() ([]netip.Addr, error) {
	if m.batchLookupSupported {
		return m.ListBatch()
	}
	return m.ListIterator()
}

// ListIterator is a iterator version of List. It is used when the map does not support batch lookup.
func (m GroupV4OuterMap) ListIterator() ([]netip.Addr, error) {
	var (
		key GroupV4Key
		val GroupV4Val
		out = make([]netip.Addr, 0, MaxGroups)
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		ip, ok := key.ToNetIPAddr()
		if !ok {
			return out, fmt.Errorf("failed to convert key to netip.Addr")
		}
		out = append(out, ip)
	}

	return out, iter.Err()
}

// ListBatch is a batched version of List. It is used when the map supports batch lookup.
func (m GroupV4OuterMap) ListBatch() ([]netip.Addr, error) {
	var (
		keys = make([]GroupV4Key, MaxGroups)
		vals = make([]GroupV4Val, MaxGroups)
		out  = make([]netip.Addr, 0, MaxGroups)
	)

	var cursor ciliumebpf.MapBatchCursor
	count := 0
	for {
		c, batchErr := m.BatchLookup(&cursor, keys, vals, nil)
		count += c
		if batchErr != nil {
			if errors.Is(batchErr, ebpf.ErrKeyNotExist) {
				break
			}
			return nil, batchErr
		}
	}

	for i := 0; i < len(keys) && i < count; i++ {
		group, ok := keys[i].ToNetIPAddr()
		if !ok {
			return nil, fmt.Errorf("failed to convert GroupV4Key.Group to netip.Addr")
		}
		out = append(out, group)
	}

	return out, nil
}

// GroupV4Key is the key for a GroupV4OuterMap
// It is a IPv4 multicast group address in big endian format.
type GroupV4Key struct {
	Group [4]byte
}

func NewGroupV4KeyFromNetIPAddr(ip netip.Addr) (out GroupV4Key, err error) {
	if !ip.Is4() || !ip.IsMulticast() {
		return out, fmt.Errorf("ip must be an IPv4 multicast address")
	}
	out.Group = ip.As4()
	return out, nil
}

func (k GroupV4Key) ToNetIPAddr() (netip.Addr, bool) {
	return netip.AddrFromSlice(k.Group[:])
}

// GroupV4Val is the value of a GroupV4OuterMap.
// It is a file descriptor for an inner SubscriberV4InnerMap.
type GroupV4Val struct {
	FD uint32
}

func OpenGroupV4OuterMap(logger *slog.Logger, name string) (*GroupV4OuterMap, error) {
	m, err := ebpf.LoadRegisterMap(logger, name)
	if err != nil {
		return nil, err
	}

	return &GroupV4OuterMap{
		Map:                  m,
		batchLookupSupported: haveBatchLookupSupport[GroupV4Key, GroupV4Val](m),
	}, nil
}

// SubscriberV4Map provides an interface between the control and data plane,
// enabling the creation, deletion, and querying of IPv4 multicast subscribers
// within a multicast group.
type SubscriberV4Map interface {
	Insert(*SubscriberV4) error
	Lookup(Src netip.Addr) (*SubscriberV4, error)
	Delete(Src netip.Addr) error
	List() ([]*SubscriberV4, error)
}

// SubscriberV4 is a multicast subscriber
type SubscriberV4 struct {
	// Source address of subscriber in big endian format
	SAddr netip.Addr
	// Interface ID of subscriber, may be a tunnel interface if subscriber
	// is remote.
	Ifindex uint32
	// Specifies if the subscriber is remote or local
	IsRemote bool
}

// SubscriberV4InnerMap is the inner map of a GroupV4OuterMap outer
// map.
//
// This map inventories all subscribers, both local and remote, for a given
// multicast group.
type SubscriberV4InnerMap struct {
	*ebpf.Map
}

func newSubscriberV4InnerMap(logger *slog.Logger) (*SubscriberV4InnerMap, error) {
	spec := newSubscriberV4InnerMapSpec()

	m := ebpf.NewMap(logger, spec)
	if err := m.OpenOrCreate(); err != nil {
		return nil, err
	}

	return &SubscriberV4InnerMap{m}, nil
}

// SubscriberV4Key is the IPv4 source address of the multicast subscriber
// in big endian format.
type SubscriberV4Key struct {
	SAddr [4]byte
}

func NewSubscriberV4KeyFromNetIPAddr(ip netip.Addr) (out SubscriberV4Key, err error) {
	if !ip.Is4() {
		return out, fmt.Errorf("ip must be IPv4")
	}
	out.SAddr = ip.As4()
	return out, nil
}

func (k SubscriberV4Key) ToNetIPAddr() (netip.Addr, bool) {
	return netip.AddrFromSlice(k.SAddr[:])
}

// SubscriberFlags are a set of flags used to further define a
// SubscriberV4
type SubscriberFlags uint32

const (
	// Flag used to define a subscriber as remote.
	// If present SubscriberV4Val.Ifindex must represent an egress interface
	// towards the remote host.
	SubscriberRemote SubscriberFlags = (1 << 0)
)

// SubscriberV4Val is a discrete subscriber value of a multicast group
// map.
type SubscriberV4Val struct {
	// Source address of subscriber in big endian format
	SourceAddr [4]byte `align:"saddr"`
	// Interface ID of subscriber, may be a tunnel interface if subscriber
	// is remote.
	Ifindex uint32 `align:"ifindex"`
	// reserved
	Pad1 uint16 `align:"pad1"`
	// reserved
	Pad2 uint8 `align:"pad2"`
	// SubscriberFlags flag bits which further a subscriber's
	// characteristics.
	Flags uint8 `align:"flags"`
}

func (v *SubscriberV4Val) ToSubsciberV4() (*SubscriberV4, error) {
	saddr, ok := SubscriberV4Key{SAddr: v.SourceAddr}.ToNetIPAddr()
	if !ok {
		return nil, fmt.Errorf("failed to convert SubscriberV4Val.SAddr to netip.Addr")
	}
	sub := &SubscriberV4{
		SAddr:   saddr,
		Ifindex: v.Ifindex,
	}
	if v.Flags != 0 {
		// only one possibility right now
		sub.IsRemote = true
	}
	return sub, nil
}

func newSubscriberV4InnerMapSpec() *ebpf.MapSpec {
	flags := bpf.GetMapMemoryFlags(ebpf.Hash)
	return &ebpf.MapSpec{
		Name:       "cilium_mcast_subscriber_v4_inner",
		Type:       ebpf.Hash,
		KeySize:    uint32(unsafe.Sizeof(SubscriberV4Key{})),
		ValueSize:  uint32(unsafe.Sizeof(SubscriberV4Val{})),
		MaxEntries: uint32(MaxSubscribers),
		Flags:      flags,
	}
}

func (m SubscriberV4InnerMap) Insert(s *SubscriberV4) error {
	key, err := NewSubscriberV4KeyFromNetIPAddr(s.SAddr)
	if err != nil {
		return err
	}

	var flags SubscriberFlags = 0
	switch {
	case s.IsRemote:
		flags |= SubscriberRemote
	}

	val := SubscriberV4Val{
		SourceAddr: key.SAddr,
		Ifindex:    s.Ifindex,
		Flags:      uint8(flags),
	}

	err = m.Update(key.SAddr, val, ciliumebpf.UpdateNoExist)
	if err != nil {
		return fmt.Errorf("failed to insert multicast subscriber: %w", err)
	}

	return nil
}

func (m SubscriberV4InnerMap) Lookup(Src netip.Addr) (*SubscriberV4, error) {
	val := SubscriberV4Val{}

	key, err := NewSubscriberV4KeyFromNetIPAddr(Src)
	if err != nil {
		return nil, err
	}

	err = m.Map.Lookup(key.SAddr, &val)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil, fmt.Errorf("no subscriber with source address %s: %w", Src.String(), err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to lookup subscriber %s: %w", Src.String(), err)
	}

	sub, err := val.ToSubsciberV4()
	if err != nil {
		return nil, err
	}

	return sub, nil
}

func (m SubscriberV4InnerMap) Delete(Src netip.Addr) error {
	key, err := NewSubscriberV4KeyFromNetIPAddr(Src)
	if err != nil {
		return err
	}
	return m.Map.Delete(key)
}

// List returns a list of all subscribers in the map. Batch lookup is used to get the subscribers.
// Minimum kernel version required for multicast is 5.13, in which batch lookup for map.Hash is supported.
func (m SubscriberV4InnerMap) List() ([]*SubscriberV4, error) {
	var (
		keys = make([]SubscriberV4Key, MaxSubscribers)
		vals = make([]SubscriberV4Val, MaxSubscribers)
		out  = make([]*SubscriberV4, 0, MaxSubscribers)
	)

	var cursor ciliumebpf.MapBatchCursor
	count := 0
	for {
		c, batchErr := m.BatchLookup(&cursor, keys, vals, nil)
		count += c
		if batchErr != nil {
			if errors.Is(batchErr, ebpf.ErrKeyNotExist) {
				break
			}
			return nil, batchErr
		}
	}

	for i := 0; i < len(vals) && i < count; i++ {
		sub, err := vals[i].ToSubsciberV4()
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}

	return out, nil
}

// haveBatchLookupSupport checks if the kernel supports batch lookup for the passed map.
func haveBatchLookupSupport[K, V any](m *ebpf.Map) bool {
	keys := make([]K, 1)
	vals := make([]V, 1)
	var cursor ciliumebpf.MapBatchCursor
	_, err := m.BatchLookup(&cursor, keys, vals, nil)
	if err != nil && errors.Is(err, ciliumebpf.ErrNotSupported) {
		return false
	}
	return true
}
