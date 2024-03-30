package cluster

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/cluster/clusterconfig/pb"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/keylock"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkstore"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type channelGroupManager struct {
	channelGroups  []*channelGroup
	proposeTimeout time.Duration
	localStorage   *localStorage
	channelKeyLock *keylock.KeyLock
	s              *Server
	wklog.Log
}

func newChannelGroupManager(s *Server) *channelGroupManager {
	return &channelGroupManager{
		proposeTimeout: s.opts.ProposeTimeout,
		s:              s,
		channelGroups:  make([]*channelGroup, s.opts.ChannelGroupCount),
		channelKeyLock: keylock.NewKeyLock(),
		Log:            wklog.NewWKLog(fmt.Sprintf("channelGroupManager[%d]", s.opts.NodeID)),
		localStorage:   s.localStorage,
	}
}

func (c *channelGroupManager) start() error {
	c.channelKeyLock.StartCleanLoop()
	var err error
	for i := 0; i < c.s.opts.ChannelGroupCount; i++ {
		cg := newChannelGroup(c.s.opts)
		err = cg.start()
		if err != nil {
			return err
		}
		c.channelGroups[i] = cg
	}
	return nil
}

func (c *channelGroupManager) stop() {

	for i := 0; i < c.s.opts.ChannelGroupCount; i++ {
		cg := c.channelGroups[i]
		cg.stop()
	}
	c.channelKeyLock.StopCleanLoop()

}

func (c *channelGroupManager) proposeMessage(ctx context.Context, channelId string, channelType uint8, log replica.Log) (messageItem, error) {

	items, err := c.proposeMessages(ctx, channelId, channelType, []replica.Log{log})
	if err != nil {
		return emptyMessageItem, err
	}
	if len(items) == 0 {
		return emptyMessageItem, errors.New("lastLogIndexs is empty")
	}
	return items[0], nil
}

func (c *channelGroupManager) proposeMessages(ctx context.Context, channelId string, channelType uint8, logs []replica.Log) ([]messageItem, error) {

	proposeMessagesCtx, proposeMessagesSpan := c.s.trace.StartSpan(ctx, "proposeMessages")
	defer proposeMessagesSpan.End()

	fetchChannelSpanCtx, fetchChannelSpan := c.s.trace.StartSpan(proposeMessagesCtx, "fetchChannel")
	defer fetchChannelSpan.End()
	fetchChannelSpan.SetString("channelId", channelId)
	fetchChannelSpan.SetUint8("channelType", channelType)
	ch, err := c.fetchChannel(fetchChannelSpanCtx, channelId, channelType)
	if err != nil {
		fetchChannelSpan.RecordError(err)
		c.Error("get channel failed", zap.Error(err))
		fetchChannelSpan.End()
		return nil, err
	}
	fetchChannelSpan.End()
	if !ch.IsLeader() { // 如果不是频道领导，则转发给频道领导
		c.Debug("not leader,forward to leader", zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint64("leaderId", ch.LeaderId()))

		_, span := c.s.trace.StartSpan(ctx, "channelProposeMessageForwardToLeader")
		span.SetString("channelId", channelId)
		span.SetUint8("channelType", channelType)
		span.SetUint64("leaderId", ch.LeaderId())
		span.SetInt("logs", len(logs))
		defer span.End()
		resp, err := c.requestChannelProposeMessage(ch.LeaderId(), channelId, channelType, logs)
		if err != nil {
			span.RecordError(err)
			c.Error("requestChannelProposeMessage failed", zap.Error(err))
			return nil, err
		}
		if resp.ClusterConfigOld {
			c.Info("local channel cluster config is old,delete it", zap.String("channelId", channelId), zap.Uint8("channelType", channelType))
			err = c.s.opts.ChannelClusterStorage.Delete(channelId, channelType)
			if err != nil {
				span.RecordError(err)
				c.Warn("deleteChannelClusterConfig failed", zap.Error(err), zap.String("channelId", channelId), zap.Uint8("channelType", channelType))
			}
		}
		return resp.MessageItems, nil
	}
	items, err := c.proposeAndWaitCommits(ctx, ch.(*channel), logs, c.proposeTimeout)
	return items, err
}

func (c *channelGroupManager) requestChannelProposeMessage(to uint64, channelId string, channelType uint8, logs []replica.Log) (*ChannelProposeResp, error) {
	node := c.s.nodeManager.node(to)
	if node == nil {
		c.Error("node is not found", zap.Uint64("nodeID", to))
		return nil, ErrNodeNotFound
	}
	timeoutCtx, cancel := context.WithTimeout(c.s.cancelCtx, c.s.opts.ReqTimeout)
	resp, err := node.requestChannelProposeMessage(timeoutCtx, &ChannelProposeReq{
		ChannelId:   channelId,
		ChannelType: channelType,
		Logs:        logs,
	})
	defer cancel()
	if err != nil {
		c.Error("requestChannelProposeMessage failed", zap.Error(err))
		return nil, err
	}
	return resp, nil
}

func (c *channelGroupManager) existChannel(channelId string, channelType uint8) bool {
	return c.channelGroup(channelId, channelType).exist(channelId, channelType)
}

func (c *channelGroupManager) fetchChannel(ctx context.Context, channelId string, channelType uint8) (ichannel, error) {

	return c.loadOrCreateChannel(ctx, channelId, channelType)
}

func (c *channelGroupManager) fetchChannelForRead(channelID string, channelType uint8) (ichannel, error) {
	return c.loadOnlyReadChannel(channelID, channelType)
}

func (c *channelGroupManager) channelGroup(channelID string, channelType uint8) *channelGroup {
	shardNo := ChannelKey(channelID, channelType)
	idx := crc32.ChecksumIEEE([]byte(shardNo)) % uint32(c.s.opts.ChannelGroupCount)
	return c.channelGroups[idx]
}

func (c *channelGroupManager) handleRecvMessage(ctx context.Context, channelID string, channelType uint8, msg replica.Message) error {

	channel, err := c.fetchChannel(ctx, channelID, channelType)
	if err != nil {
		return err
	}
	if channel == nil {
		return ErrChannelNotFound
	}
	return channel.handleRecvMessage(msg)
}

func (c *channelGroupManager) loadOrCreateChannel(ctx context.Context, channelID string, channelType uint8) (ichannel, error) {

	var (
		channel ichannel
		err     error
	)

	slotId := c.s.getChannelSlotId(channelID)
	slot := c.s.clusterEventListener.clusterconfigManager.slot(slotId)
	if slot == nil {
		return nil, ErrSlotNotExist
	}
	if slot.Leader == c.s.opts.NodeID { // 当前节点是槽位的leader，槽节点有任命频道领导的权限，槽节点保存属于此槽频道的分布式配置

		channel, err = c.getChannelForSlotLeader(ctx, channelID, channelType)
		if err != nil {
			c.Error("getChannelForSlotLeader failed", zap.Error(err), zap.String("channelId", channelID), zap.Uint8("channelType", channelType))
			return nil, err
		}
	} else {

		channel, err = c.getChannelForOthers(ctx, channelID, channelType)
		if err != nil {
			c.Error("getChannelForOthers failed", zap.Error(err))
			return nil, err
		}
	}
	if channel == nil {
		return nil, fmt.Errorf("not found channel")
	}
	return channel, nil
}

// 加载仅仅可读的频道（不会激活频道）
func (c *channelGroupManager) loadOnlyReadChannel(channelId string, channelType uint8) (ichannel, error) {

	shardNo := ChannelKey(channelId, channelType)
	c.channelKeyLock.Lock(shardNo)
	defer c.channelKeyLock.Unlock(shardNo)

	cacheChannel := c.channelGroup(channelId, channelType).channel(channelId, channelType)
	if cacheChannel != nil {
		return cacheChannel, nil
	}
	slotId := c.s.getChannelSlotId(channelId)
	slot := c.s.clusterEventListener.clusterconfigManager.slot(slotId)
	if slot == nil {
		return nil, ErrSlotNotExist
	}

	clusterConfig, err := c.s.opts.ChannelClusterStorage.Get(channelId, channelType)
	if err != nil {
		return nil, err
	}

	if slot.Leader == c.s.opts.NodeID {
		if clusterConfig == nil {
			c.Error("channel cluster config is not found", zap.String("channelId", channelId), zap.Uint8("channelType", channelType))
			return nil, fmt.Errorf("channel cluster config is not found")
		}
	} else if clusterConfig == nil {
		clusterConfig, err = c.requestChannelClusterConfigFromSlotLeader(channelId, channelType)
		if err != nil {
			return nil, err
		}
		if clusterConfig != nil {
			err = c.s.opts.ChannelClusterStorage.Save(channelId, channelType, clusterConfig)
			if err != nil {
				return nil, err
			}
		}
	}
	if clusterConfig == nil {
		c.Error("channel cluster config is not found", zap.String("channelId", channelId), zap.Uint8("channelType", channelType))
		return nil, fmt.Errorf("channel cluster config is not found")
	}
	return newProxyChannel(c.s.opts.NodeID, clusterConfig), nil
}

func (c *channelGroupManager) getChannelForSlotLeader(ctx context.Context, channelID string, channelType uint8) (ichannel, error) {
	channel := c.channelGroup(channelID, channelType).channel(channelID, channelType)
	if channel != nil {
		return channel, nil
	}
	shardNo := ChannelKey(channelID, channelType)
	c.channelKeyLock.Lock(shardNo)
	defer c.channelKeyLock.Unlock(shardNo)

	spanCtx, span := c.s.trace.StartSpan(ctx, "createChannelForSlotLeader")
	defer span.End()
	span.SetString("channelId", channelID)
	span.SetUint8("channelType", channelType)
	span.SetBool("hasClusterConfig", true)
	clusterconfig, err := c.s.opts.ChannelClusterStorage.Get(channelID, channelType)
	if err != nil {
		return nil, err
	}
	if clusterconfig == nil { // 没有集群信息则创建一个新的集群信息
		span.SetBool("hasClusterConfig", false)
		clusterconfig, err = c.createChannelClusterInfo(channelID, channelType) // 如果槽领导节点不存在频道集群配置，那么此频道集群一定没初始化（注意：是一定没初始化），所以创建一个初始化集群配置
		if err != nil {
			c.Error("create channel cluster info failed", zap.Error(err))
			span.RecordError(err)
			return nil, err
		}

	}
	channel, err = c.newChannelByClusterInfo(clusterconfig)
	if err != nil {
		c.Error("newChannelByClusterInfo failed", zap.Error(err))
		span.RecordError(err)
		return nil, err
	}
	if channel == nil {
		span.RecordError(errors.New("new channel failed"))
		return nil, fmt.Errorf("new channel failed")
	}
	// 检查在线副本是否超过半数
	// if !c.checkOnlineReplicaCount(clusterconfig) {
	// 	return nil, errors.New("online replica count is not enough, checkOnlineReplicaCount and createChannelClusterInfo failed")
	// }

	// 保存分布式配置
	err = c.s.opts.ChannelClusterStorage.Save(clusterconfig.ChannelID, clusterconfig.ChannelType, clusterconfig)
	if err != nil {
		c.Error("proposeChannelClusterConfig failed", zap.Error(err))
		span.RecordError(err)
		return nil, err
	}
	channel.updateClusterConfig(clusterconfig)
	// // 通知任命领导
	// err = c.notifyAppointLeader(clusterconfig, nil)
	// if err != nil {
	// 	c.Error("notifyAppointLeader failed", zap.Error(err))
	// 	return nil, err
	// }
	err = c.electionIfNeed(spanCtx, channel) // 根据需要是否进行选举
	if err != nil {
		span.RecordError(err)
		c.Error("electionIfNeed failed", zap.Error(err))
		return nil, err
	}
	// 添加到channelGroup
	c.channelGroup(channelID, channelType).add(channel)

	return channel, err
}

func (c *channelGroupManager) getChannelForOthers(ctx context.Context, channelID string, channelType uint8) (ichannel, error) {

	cacheChannel := c.channelGroup(channelID, channelType).channel(channelID, channelType)
	if cacheChannel != nil {
		return cacheChannel, nil
	}

	shardNo := ChannelKey(channelID, channelType)
	c.channelKeyLock.Lock(shardNo)
	defer c.channelKeyLock.Unlock(shardNo)

	_, span := c.s.trace.StartSpan(ctx, "getChannelForOthers")
	defer span.End()
	span.SetString("channelId", channelID)
	span.SetUint8("channelType", channelType)

	clusterConfig, err := c.s.opts.ChannelClusterStorage.Get(channelID, channelType)
	if err != nil {
		return nil, err
	}

	if clusterConfig == nil || clusterConfig.LeaderId == 0 {
		clusterConfig, err = c.requestChannelClusterConfigFromSlotLeader(channelID, channelType) // 从频道所在槽的领导节点获取频道分布式配置
		if err != nil {
			return nil, err
		}
		if clusterConfig != nil {
			err = c.s.opts.ChannelClusterStorage.Save(channelID, channelType, clusterConfig)
			if err != nil {
				return nil, err
			}
		}
	}

	var (
		ch ichannel
	)
	if wkutil.ArrayContainsUint64(clusterConfig.Replicas, c.s.opts.NodeID) { // 如果当前节点是频道的副本，则创建频道集群
		ch, err = c.newChannelByClusterInfo(clusterConfig)
		if err != nil {
			return nil, err
		}
		ch.(*channel).updateClusterConfig(clusterConfig)
		c.channelGroup(channelID, channelType).add(ch.(*channel))
	} else { // 如果当前节点不是频道的副本，则创建一个代理频道
		ch = newProxyChannel(c.s.opts.NodeID, clusterConfig)
	}

	return ch, nil
}

// 从频道所在槽获取频道的分布式信息
func (c *channelGroupManager) requestChannelClusterConfigFromSlotLeader(channelId string, channelType uint8) (*wkstore.ChannelClusterConfig, error) {
	slotId := c.s.getChannelSlotId(channelId)
	slot := c.s.clusterEventListener.clusterconfigManager.slot(slotId)
	if slot == nil {
		return nil, ErrSlotNotExist
	}
	node := c.s.nodeManager.node(slot.Leader)
	if node == nil {
		return nil, fmt.Errorf("not found slot leader node")
	}
	timeoutCtx, cancel := context.WithTimeout(c.s.cancelCtx, c.s.opts.ReqTimeout)
	defer cancel()
	clusterConfig, err := node.requestChannelClusterConfig(timeoutCtx, &ChannelClusterConfigReq{
		ChannelID:   channelId,
		ChannelType: channelType,
	})
	if err != nil {
		c.Error("requestChannelClusterConfigFromSlotLeader failed", zap.Error(err), zap.Uint64("slotLeader", slot.Leader), zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint32("slotId", slotId))
		return nil, err
	}
	return clusterConfig, nil
}

// 进行频道选举
func (c *channelGroupManager) createChannelClusterInfo(channelID string, channelType uint8) (*wkstore.ChannelClusterConfig, error) {
	allowVoteNodes := c.s.clusterEventListener.clusterconfigManager.allowVoteNodes()
	shardNo := ChannelKey(channelID, channelType)
	lastTerm, err := c.s.localStorage.leaderLastTerm(shardNo)
	if err != nil {
		return nil, err
	}

	clusterConfig := &wkstore.ChannelClusterConfig{
		ChannelID:    channelID,
		ChannelType:  channelType,
		ReplicaCount: c.s.opts.ChannelMaxReplicaCount,
		Term:         lastTerm,
	}
	replicaIDs := make([]uint64, 0, c.s.opts.ChannelMaxReplicaCount)

	replicaIDs = append(replicaIDs, c.s.opts.NodeID)

	// 随机选择副本
	newOnlineNodes := make([]*pb.Node, 0, len(allowVoteNodes))
	newOnlineNodes = append(newOnlineNodes, allowVoteNodes...)
	rand.Shuffle(len(newOnlineNodes), func(i, j int) {
		newOnlineNodes[i], newOnlineNodes[j] = newOnlineNodes[j], newOnlineNodes[i]
	})

	for _, onlineNode := range newOnlineNodes {
		if onlineNode.Id == c.s.opts.NodeID {
			continue
		}
		replicaIDs = append(replicaIDs, onlineNode.Id)
		if len(replicaIDs) >= int(c.s.opts.ChannelMaxReplicaCount) {
			break
		}
	}
	clusterConfig.Replicas = replicaIDs
	return clusterConfig, nil
}

// 是否需要进行选举
func (c *channelGroupManager) needElection(clusterConfig *wkstore.ChannelClusterConfig) (bool, error) {
	slotId := c.s.getChannelSlotId(clusterConfig.ChannelID)
	slot := c.s.clusterEventListener.clusterconfigManager.slot(slotId)
	if slot == nil {
		return false, ErrSlotNotExist
	}
	if slot.Leader != c.s.opts.NodeID { // 频道所在槽的领导不是当前节点(只有频道所在槽的领导才有权限进行选举)
		c.Debug("slot leader is not current node", zap.Uint64("slotLeader", slot.Leader), zap.Uint64("nodeID", c.s.opts.NodeID))
		return false, nil
	}
	if clusterConfig.LeaderId != 0 {
		node := c.s.clusterEventListener.clusterconfigManager.node(clusterConfig.LeaderId)
		if node == nil {
			return false, errors.New("leader node is not found")
		}
		if node.Online { // 领导在线，不需要进行选举
			return false, nil
		}
	}

	return true, nil
}

// 进行选举
func (c *channelGroupManager) electionIfNeed(ctx context.Context, channel *channel) error {
	clusterConfig := channel.clusterConfig
	if clusterConfig == nil {
		return errors.New("channel clusterConfig is not found")
	}
	channelId := clusterConfig.ChannelID
	channelType := clusterConfig.ChannelType

	needElection, err := c.needElection(clusterConfig)
	if err != nil {
		return err
	}
	if !needElection { // 不需要选举
		return nil
	}

	_, span := c.s.trace.StartSpan(ctx, "election")
	defer span.End()

	// 检查在线副本是否超过半数
	if !c.checkOnlineReplicaCount(clusterConfig) {
		span.RecordError(errors.New("1checkOnlineReplicaCount:online replica count is not enough"))
		c.Error("1checkOnlineReplicaCount: online replica count is not enough", zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint64s("replicas", clusterConfig.Replicas), zap.Int("onlineReplicaCount", len(clusterConfig.Replicas)), zap.Int("quorum", c.quorum()))
		return errors.New("1online replica count is not enough, checkOnlineReplicaCount failed")
	}

	// 获取参选投票频道最后一条消息的索引
	channelLogInfoMap, err := c.requestChannelLastLogInfos(clusterConfig)
	if err != nil {
		span.RecordError(err)
		return err
	}
	if len(channelLogInfoMap) < c.quorum() {
		span.RecordError(errors.New("online replica count is not enough"))
		c.Error("replica count is not enough", zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint64s("replicas", clusterConfig.Replicas), zap.Int("onlineReplicaCount", len(clusterConfig.Replicas)), zap.Int("quorum", c.quorum()))
		return errors.New("online replica count is not enough")
	}

	// 从参选的日志信息里选举出新的领导
	newLeaderID := c.channelLeaderIDByLogInfo(channelLogInfoMap)
	if newLeaderID == 0 {
		span.RecordError(errors.New("new leader is not found"))
		return errors.New("new leader is not found")
	}
	clusterConfig.LeaderId = newLeaderID
	clusterConfig.Term = clusterConfig.Term + 1 // 任期加1

	span.SetUint64("newLeaderID", newLeaderID)
	span.SetUint32("term", clusterConfig.Term)
	span.SetUint64s("replicas", clusterConfig.Replicas)
	channel.setLeaderId(newLeaderID)

	c.Info("成功选举出新的领导", zap.Uint64("newLeaderID", clusterConfig.LeaderId), zap.Uint32("term", clusterConfig.Term), zap.Uint64s("replicas", clusterConfig.Replicas), zap.String("channelId", channelId), zap.Uint8("channelType", channelType))

	// 保存分布式配置
	err = c.s.opts.ChannelClusterStorage.Save(channelId, channelType, clusterConfig)
	if err != nil {
		c.Error("saveChannelClusterConfig failed", zap.Error(err))
		span.RecordError(err)
		return err
	}
	channel.updateClusterConfig(clusterConfig)

	// 发送任命消息给频道所有副本
	// err = c.notifyAppointLeader(clusterConfig, channel)
	// if err != nil {
	// 	c.Error("notifyAppointLeader failed", zap.Error(err))
	// 	return err
	// }

	return nil
}

func (c *channelGroupManager) advanceHandler(channelId string, channelType uint8) func() {

	return func() {
		c.channelGroup(channelId, channelType).listener.advance()
	}
}

func (c *channelGroupManager) newChannelByClusterInfo(channelClusterInfo *wkstore.ChannelClusterConfig) (*channel, error) {
	shardNo := ChannelKey(channelClusterInfo.ChannelID, channelClusterInfo.ChannelType)
	// 获取当前节点已应用的日志
	appliedIndex, err := c.localStorage.getAppliedIndex(shardNo)
	if err != nil {
		return nil, err
	}
	channel := newChannel(channelClusterInfo, appliedIndex, c.localStorage, c.advanceHandler(channelClusterInfo.ChannelID, channelClusterInfo.ChannelType), c.s.opts)
	return channel, nil
}

func (c *channelGroupManager) requestChannelAppointLeader(clusterConfig *wkstore.ChannelClusterConfig) error {
	allowVoteNodes := c.s.clusterEventListener.clusterconfigManager.allowVoteNodes()
	if len(allowVoteNodes) == 0 {
		return errors.New("allowVoteNodes is empty")
	}

	appointResults := make([]uint64, 0)
	timeoutCtx, cancel := context.WithTimeout(c.s.cancelCtx, c.s.opts.ReqTimeout)
	defer cancel()
	requestGroup, ctx := errgroup.WithContext(timeoutCtx)

	for _, allowVoteNode := range allowVoteNodes {
		if !c.s.clusterEventListener.clusterconfigManager.nodeIsOnline(allowVoteNode.Id) {
			c.Warn("node is not online", zap.Uint64("nodeID", allowVoteNode.Id))
			continue
		}
		if allowVoteNode.Id == c.s.opts.NodeID {
			appointResults = append(appointResults, allowVoteNode.Id)
			continue
		}

		requestGroup.Go(func(n *pb.Node, config *wkstore.ChannelClusterConfig) func() error {
			return func() error {
				nodecli := c.s.nodeManager.node(n.Id)
				if nodecli == nil {
					c.Error("node is not found", zap.Uint64("nodeID", n.Id))
					return nil
				}
				err := nodecli.requestChannelAppointLeader(ctx, &AppointLeaderReq{
					ChannelId:   config.ChannelID,
					ChannelType: config.ChannelType,
					LeaderId:    config.LeaderId,
					Term:        config.Term,
				})
				if err != nil {
					c.Error("requestChannelAppointLeader failed", zap.Error(err))
					return nil
				}
				appointResults = append(appointResults, n.Id)
				return nil
			}
		}(allowVoteNode, clusterConfig))
	}
	_ = requestGroup.Wait()

	if len(appointResults) < c.quorum() {
		c.Error("appoint leader failed", zap.Int("appointResults", len(appointResults)))
		return errors.New("appoint leader failed, appointResults is not enough")
	}
	return nil
}

// 检查在线副本是否超过半数
func (c *channelGroupManager) checkOnlineReplicaCount(clusterConfig *wkstore.ChannelClusterConfig) bool {
	onlineReplicaCount := 0
	for _, replicaID := range clusterConfig.Replicas {
		if replicaID == c.s.opts.NodeID {
			onlineReplicaCount++
			continue
		}
		node := c.s.clusterEventListener.clusterconfigManager.node(replicaID)
		if node != nil && node.Online {
			onlineReplicaCount++
		}
	}
	return onlineReplicaCount >= c.quorum()
}

func (c *channelGroupManager) quorum() int {
	if c.s.opts.IsSingleNode() {
		return 1
	}
	return int(c.s.opts.ChannelMaxReplicaCount/2) + 1
}

// 通过日志高度选举频道领导
func (c *channelGroupManager) channelLeaderIDByLogInfo(channelLogInfoMap map[uint64]*ChannelLastLogInfoResponse) uint64 {
	var leaderID uint64 = 0
	var leaderLogIndex uint64 = 0
	for nodeID, resp := range channelLogInfoMap {
		if resp.LogIndex > leaderLogIndex {
			leaderID = nodeID
			leaderLogIndex = resp.LogIndex
		}
	}
	if leaderID != c.s.opts.NodeID {
		resp := channelLogInfoMap[c.s.opts.NodeID]
		if resp.LogIndex >= leaderLogIndex { // 如果选举出来的领导日志高度和当前节点日志高度一样，那么当前节点优先成为领导
			leaderID = c.s.opts.NodeID
		}
	}
	return leaderID
}

// 获取频道最后一条消息的索引
func (c *channelGroupManager) requestChannelLastLogInfos(clusterInfo *wkstore.ChannelClusterConfig) (map[uint64]*ChannelLastLogInfoResponse, error) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	requestGroup, ctx := errgroup.WithContext(timeoutCtx)
	shardNo := ChannelKey(clusterInfo.ChannelID, clusterInfo.ChannelType)
	channelLogInfoMap := make(map[uint64]*ChannelLastLogInfoResponse, 0)
	channelLogInfoMapLock := new(sync.Mutex)

	for _, replicaID := range clusterInfo.Replicas {
		if !c.s.clusterEventListener.clusterconfigManager.nodeIsOnline(replicaID) {
			continue
		}
		if replicaID == c.s.opts.NodeID {
			lastLogIndex, err := c.s.opts.MessageLogStorage.LastIndex(shardNo)
			if err != nil {
				return nil, err
			}
			channelLogInfoMapLock.Lock()
			channelLogInfoMap[replicaID] = &ChannelLastLogInfoResponse{
				LogIndex: lastLogIndex,
			}
			channelLogInfoMapLock.Unlock()
			continue
		} else {
			requestGroup.Go(func(rcID uint64) func() error {
				return func() error {
					node := c.s.nodeManager.node(rcID)
					if node == nil {
						c.Warn("node is not found", zap.Uint64("nodeID", rcID))
						return nil
					}
					resp, err := node.requestChannelLastLogInfo(ctx, &ChannelLastLogInfoReq{
						ChannelID:   clusterInfo.ChannelID,
						ChannelType: clusterInfo.ChannelType,
					})
					if err != nil {
						c.Warn("requestChannelLastLogInfo failed", zap.Uint64("nodeId", rcID), zap.String("channelId", clusterInfo.ChannelID), zap.Uint8("channelType", clusterInfo.ChannelType), zap.Error(err))
						return nil
					}
					channelLogInfoMapLock.Lock()
					channelLogInfoMap[rcID] = resp
					channelLogInfoMapLock.Unlock()
					return nil
				}
			}(replicaID))

		}
	}
	_ = requestGroup.Wait()

	return channelLogInfoMap, nil
}

func (c *channelGroupManager) proposeAndWaitCommits(ctx context.Context, ch *channel, logs []replica.Log, timeout time.Duration) ([]messageItem, error) {
	return ch.proposeAndWaitCommits(ctx, logs, timeout)
}