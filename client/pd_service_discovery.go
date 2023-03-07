// Copyright 2019 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pd

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/client/errs"
	"github.com/tikv/pd/client/grpcutil"
	"github.com/tikv/pd/client/tlsutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	globalDCLocation     = "global"
	memberUpdateInterval = time.Minute
)

// ServiceDiscovery defines the general interface for service discovery on a quorum-based cluster
// or a primary/secondary configured cluster.
type ServiceDiscovery interface {
	// Init initialize the concrete client underlying
	Init() error
	// Close releases all resources
	Close()
	// GetClusterID returns the ID of the cluster
	GetClusterID(context.Context) uint64
	// GetURLs returns the URLs of the servers.
	GetURLs() []string
	// GetServingEndpointClientConn returns the grpc client connection of the serving endpoint
	// which is the leader in a quorum-based cluster or the primary in a primary/secondary
	// configured cluster.
	GetServingEndpointClientConn() *grpc.ClientConn
	// GetClientConns returns the mapping {addr -> a gRPC connection}
	GetClientConns() *sync.Map
	// GetServingAddr returns the serving endpoint which is the leader in a quorum-based cluster
	// or the primary in a primary/secondary configured cluster.
	GetServingAddr() string
	// GetBackupAddrs gets the addresses of the current reachable and healthy backup service
	// endpoints randomly. Backup service endpoints are followers in a quorum-based cluster or
	// secondaries in a primary/secondary configured cluster.
	GetBackupAddrs() []string
	// GetOrCreateGRPCConn returns the corresponding grpc client connection of the given addr
	GetOrCreateGRPCConn(addr string) (*grpc.ClientConn, error)
	// ScheduleCheckMemberChanged is used to trigger a check to see if there is any membership change
	// among the leader/followers in a quorum-based cluster or among the primary/secondaries in a
	// primary/secondary configured cluster.
	ScheduleCheckMemberChanged()
	// CheckMemberChanged immediately check if there is any membership change among the leader/followers
	// in a quorum-based cluster or among the primary/secondaries in a primary/secondary configured cluster.
	CheckMemberChanged() error
	// AddServingAddrSwitchedCallback adds callbacks which will be called when the leader
	// in a quorum-based cluster or the primary in a primary/secondary configured cluster
	// is switched.
	AddServingAddrSwitchedCallback(callbacks ...func())
	// AddServiceAddrsSwitchedCallback adds callbacks which will be called when any leader/follower
	// in a quorum-based cluster or any primary/secondary in a primary/secondary configured cluster
	// is changed.
	AddServiceAddrsSwitchedCallback(callbacks ...func())
}

type tsoServAddrsUpdatedFunc func(map[string]string) error

type tsoAllocatorEventSource interface {
	// AddTSOServAddrsUpdatedCallback adds callbacks which will be called
	// when the global/local tso allocator leader list is updated.
	AddTSOServAddrsUpdatedCallback(callbacks ...tsoServAddrsUpdatedFunc)
}

var _ ServiceDiscovery = (*pdServiceDiscovery)(nil)
var _ tsoAllocatorEventSource = (*pdServiceDiscovery)(nil)

// pdServiceDiscovery is the service discovery client of PD/API service which is quorum based
type pdServiceDiscovery struct {
	isInitialized bool

	urls atomic.Value // Store as []string
	// PD leader URL
	leader atomic.Value // Store as string
	// PD follower URLs
	followers atomic.Value // Store as []string

	clusterID uint64
	// addr -> a gRPC connection
	clientConns sync.Map // Store as map[string]*grpc.ClientConn

	// leaderSwitchedCbs will be called after the leader swichted
	leaderSwitchedCbs []func()
	// membersChangedCbs will be called after there is any membership change in the
	// leader and followers
	membersChangedCbs []func()
	// tsoAllocLeadersUpdatedCbs will be called when the global/local tso allocator
	// leader list is updated. The input is a map {DC Localtion -> Leader Addr}
	tsoAllocLeadersUpdatedCbs []tsoServAddrsUpdatedFunc

	checkMembershipCh chan struct{}

	wg        *sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	tlsCfg *tlsutil.TLSConfig
	// Client option.
	option *option
}

// newPDServiceDiscovery returns a new baseClient.
func newPDServiceDiscovery(ctx context.Context, cancel context.CancelFunc,
	wg *sync.WaitGroup, urls []string, tlsCfg *tlsutil.TLSConfig, option *option) *pdServiceDiscovery {
	pdsd := &pdServiceDiscovery{
		checkMembershipCh: make(chan struct{}, 1),
		ctx:               ctx,
		cancel:            cancel,
		wg:                wg,
		tlsCfg:            tlsCfg,
		option:            option,
	}
	pdsd.urls.Store(urls)
	return pdsd
}

func (c *pdServiceDiscovery) Init() error {
	if !c.isInitialized {
		if err := c.initRetry(c.initClusterID); err != nil {
			c.cancel()
			return err
		}
		if err := c.initRetry(c.updateMember); err != nil {
			c.cancel()
			return err
		}
		log.Info("[pd] init cluster id", zap.Uint64("cluster-id", c.clusterID))

		c.wg.Add(1)
		go c.memberLoop()

		c.isInitialized = true
	}

	return nil
}

func (c *pdServiceDiscovery) initRetry(f func() error) error {
	var err error
	for i := 0; i < c.option.maxRetryTimes; i++ {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-c.ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return errors.WithStack(err)
}

func (c *pdServiceDiscovery) memberLoop() {
	defer c.wg.Done()

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	for {
		select {
		case <-c.checkMembershipCh:
		case <-time.After(memberUpdateInterval):
		case <-ctx.Done():
			return
		}
		failpoint.Inject("skipUpdateMember", func() {
			failpoint.Continue()
		})
		if err := c.updateMember(); err != nil {
			log.Error("[pd] failed to update member", errs.ZapError(err))
		}
	}
}

// Close releases all resources
func (c *pdServiceDiscovery) Close() {
	c.closeOnce.Do(func() {
		c.clientConns.Range(func(key, cc interface{}) bool {
			if err := cc.(*grpc.ClientConn).Close(); err != nil {
				log.Error("[pd] failed to close gRPC clientConn", errs.ZapError(errs.ErrCloseGRPCConn, err))
			}
			c.clientConns.Delete(key)
			return true
		})
	})
}

// GetClusterID returns the ClusterID.
func (c *pdServiceDiscovery) GetClusterID(context.Context) uint64 {
	return c.clusterID
}

// GetURLs returns the URLs of the servers.
// For testing use. It should only be called when the client is closed.
func (c *pdServiceDiscovery) GetURLs() []string {
	return c.urls.Load().([]string)
}

// GetServingAddr returns the grpc client connection of the serving endpoint
// which is the leader in a quorum-based cluster or the primary in a primary/secondary
// configured cluster.
func (c *pdServiceDiscovery) GetServingEndpointClientConn() *grpc.ClientConn {
	if cc, ok := c.clientConns.Load(c.getLeaderAddr()); ok {
		return cc.(*grpc.ClientConn)
	}
	return nil
}

// GetClientConns returns the mapping {addr -> a gRPC connection}
func (c *pdServiceDiscovery) GetClientConns() *sync.Map {
	return &c.clientConns
}

// GetServingAddr returns the leader address
func (c *pdServiceDiscovery) GetServingAddr() string {
	return c.getLeaderAddr()
}

// GetBackupAddrs gets the addresses of the current reachable and healthy followers
// in a quorum-based cluster.
func (c *pdServiceDiscovery) GetBackupAddrs() []string {
	return c.getFollowerAddrs()
}

// ScheduleCheckMemberChanged is used to check if there is any membership
// change among the leader and the followers.
func (c *pdServiceDiscovery) ScheduleCheckMemberChanged() {
	select {
	case c.checkMembershipCh <- struct{}{}:
	default:
	}
}

// Immediately check if there is any membership change among the leader/followers in a
// quorum-based cluster or among the primary/secondaries in a primary/secondary configured cluster.
func (c *pdServiceDiscovery) CheckMemberChanged() error {
	return c.updateMember()
}

// AddServingAddrSwitchedCallback adds callbacks which will be called
// when the leader is switched.
func (c *pdServiceDiscovery) AddServingAddrSwitchedCallback(callbacks ...func()) {
	c.leaderSwitchedCbs = append(c.leaderSwitchedCbs, callbacks...)
}

// AddServiceAddrsSwitchedCallback adds callbacks which will be called when
// any leader/follower is changed.
func (c *pdServiceDiscovery) AddServiceAddrsSwitchedCallback(callbacks ...func()) {
	c.membersChangedCbs = append(c.membersChangedCbs, callbacks...)
}

// AddTSOServAddrsUpdatedCallback adds callbacks which will be called
// when the global/local tso allocator leader list is updated.
func (c *pdServiceDiscovery) AddTSOServAddrsUpdatedCallback(callbacks ...tsoServAddrsUpdatedFunc) {
	c.tsoAllocLeadersUpdatedCbs = append(c.tsoAllocLeadersUpdatedCbs, callbacks...)
}

// getLeaderAddr returns the leader address.
func (c *pdServiceDiscovery) getLeaderAddr() string {
	leaderAddr := c.leader.Load()
	if leaderAddr == nil {
		return ""
	}
	return leaderAddr.(string)
}

// getFollowerAddrs returns the follower address.
func (c *pdServiceDiscovery) getFollowerAddrs() []string {
	followerAddrs := c.followers.Load()
	if followerAddrs == nil {
		return []string{}
	}
	return followerAddrs.([]string)
}

func (c *pdServiceDiscovery) initClusterID() error {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	var clusterID uint64
	for _, u := range c.GetURLs() {
		members, err := c.getMembers(ctx, u, c.option.timeout)
		if err != nil || members.GetHeader() == nil {
			log.Warn("[pd] failed to get cluster id", zap.String("url", u), errs.ZapError(err))
			continue
		}
		if clusterID == 0 {
			clusterID = members.GetHeader().GetClusterId()
			continue
		}
		failpoint.Inject("skipClusterIDCheck", func() {
			failpoint.Continue()
		})
		// All URLs passed in should have the same cluster ID.
		if members.GetHeader().GetClusterId() != clusterID {
			return errors.WithStack(errUnmatchedClusterID)
		}
	}
	// Failed to init the cluster ID.
	if clusterID == 0 {
		return errors.WithStack(errFailInitClusterID)
	}
	c.clusterID = clusterID
	return nil
}

func (c *pdServiceDiscovery) updateMember() error {
	for i, u := range c.GetURLs() {
		failpoint.Inject("skipFirstUpdateMember", func() {
			if i == 0 {
				failpoint.Continue()
			}
		})
		members, err := c.getMembers(c.ctx, u, updateMemberTimeout)
		// Check the cluster ID.
		if err == nil && members.GetHeader().GetClusterId() != c.clusterID {
			err = errs.ErrClientUpdateMember.FastGenByArgs("cluster id does not match")
		}
		// Check the TSO Allocator Leader.
		var errTSO error
		if err == nil {
			if members.GetLeader() == nil || len(members.GetLeader().GetClientUrls()) == 0 {
				err = errs.ErrClientGetLeader.FastGenByArgs("leader address don't exist")
			}
			// Still need to update TsoAllocatorLeaders, even if there is no PD leader
			errTSO = c.switchTSOAllocatorLeaders(members.GetTsoAllocatorLeaders())
		}

		// Failed to get members
		if err != nil {
			log.Info("[pd] cannot update member from this address",
				zap.String("address", u),
				errs.ZapError(err))
			select {
			case <-c.ctx.Done():
				return errors.WithStack(err)
			default:
				continue
			}
		}

		c.updateURLs(members.GetMembers())
		c.updateFollowers(members.GetMembers(), members.GetLeader())
		if err := c.switchLeader(members.GetLeader().GetClientUrls()); err != nil {
			return err
		}

		// If `switchLeader` succeeds but `switchTSOAllocatorLeader` has an error,
		// the error of `switchTSOAllocatorLeader` will be returned.
		return errTSO
	}
	return errs.ErrClientGetMember.FastGenByArgs(c.GetURLs())
}

func (c *pdServiceDiscovery) getMembers(ctx context.Context, url string, timeout time.Duration) (*pdpb.GetMembersResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cc, err := c.GetOrCreateGRPCConn(url)
	if err != nil {
		return nil, err
	}
	members, err := pdpb.NewPDClient(cc).GetMembers(ctx, &pdpb.GetMembersRequest{})
	if err != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s", err, cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMember.Wrap(attachErr).GenWithStackByCause()
	}
	if members.GetHeader().GetError() != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s", members.GetHeader().GetError().String(), cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMember.Wrap(attachErr).GenWithStackByCause()
	}
	return members, nil
}

func (c *pdServiceDiscovery) updateURLs(members []*pdpb.Member) {
	urls := make([]string, 0, len(members))
	for _, m := range members {
		urls = append(urls, m.GetClientUrls()...)
	}

	sort.Strings(urls)
	oldURLs := c.GetURLs()
	// the url list is same.
	if reflect.DeepEqual(oldURLs, urls) {
		return
	}
	c.urls.Store(urls)
	// Update the connection contexts when member changes if TSO Follower Proxy is enabled.
	if c.option.getEnableTSOFollowerProxy() {
		// Run callbacks to refelect the membership changes in the leader and followers.
		for _, cb := range c.membersChangedCbs {
			cb()
		}
	}
	log.Info("[pd] update member urls", zap.Strings("old-urls", oldURLs), zap.Strings("new-urls", urls))
}

func (c *pdServiceDiscovery) switchLeader(addrs []string) error {
	// FIXME: How to safely compare leader urls? For now, only allows one client url.
	addr := addrs[0]
	oldLeader := c.getLeaderAddr()
	if addr == oldLeader {
		return nil
	}

	if _, err := c.GetOrCreateGRPCConn(addr); err != nil {
		log.Warn("[pd] failed to connect leader", zap.String("leader", addr), errs.ZapError(err))
		return err
	}
	// Set PD leader and Global TSO Allocator (which is also the PD leader)
	c.leader.Store(addr)
	c.switchTSOAllocatorLeader(globalDCLocation, addr)
	// Run callbacks
	for _, cb := range c.leaderSwitchedCbs {
		cb()
	}
	log.Info("[pd] switch leader", zap.String("new-leader", addr), zap.String("old-leader", oldLeader))
	return nil
}

func (c *pdServiceDiscovery) updateFollowers(members []*pdpb.Member, leader *pdpb.Member) {
	var addrs []string
	for _, member := range members {
		if member.GetMemberId() != leader.GetMemberId() {
			if len(member.GetClientUrls()) > 0 {
				addrs = append(addrs, member.GetClientUrls()...)
			}
		}
	}
	c.followers.Store(addrs)
}

func (c *pdServiceDiscovery) switchTSOAllocatorLeaders(allocatorMap map[string]*pdpb.Member) error {
	if len(allocatorMap) == 0 {
		return nil
	}

	allocMap := make(map[string]string)
	// Switch to the new one
	for dcLocation, member := range allocatorMap {
		if len(member.GetClientUrls()) == 0 {
			continue
		}
		allocMap[dcLocation] = member.GetClientUrls()[0]
	}

	// Run callbacks to refelect any possible change in the local tso allocators.
	for _, cb := range c.tsoAllocLeadersUpdatedCbs {
		if err := cb(allocMap); err != nil {
			return err
		}
	}

	return nil
}

func (c *pdServiceDiscovery) switchTSOAllocatorLeader(dcLocation string, addr string) error {
	allocMap := map[string]string{dcLocation: addr}

	// Run callbacks to refelect any possible change in the global/local tso allocator.
	for _, cb := range c.tsoAllocLeadersUpdatedCbs {
		if err := cb(allocMap); err != nil {
			return err
		}
	}

	return nil
}

// GetOrCreateGRPCConn returns the corresponding grpc client connection of the given addr
func (c *pdServiceDiscovery) GetOrCreateGRPCConn(addr string) (*grpc.ClientConn, error) {
	return grpcutil.GetOrCreateGRPCConn(c.ctx, &c.clientConns, addr, c.tlsCfg, c.option.gRPCDialOptions...)
}
