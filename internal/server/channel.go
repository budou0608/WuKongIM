package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/wkdb"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkserver/proto"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	wkproto "github.com/WuKongIM/WuKongIMGoProto"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type channel struct {
	uniqueNo string

	key         string
	channelId   string
	channelType uint8

	info wkdb.ChannelInfo // 频道基础信息

	msgQueue *channelMsgQueue // 消息队列
	streams  *streamList      // 流消息集合

	actions []*ChannelAction

	tmpSubscribers     []string // 临时订阅者
	tmpSubscribersLock sync.RWMutex

	// options
	storageMaxSize uint64 // 每次存储的最大字节数量
	deliverMaxSize uint64 // 每次投递的最大字节数量

	forwardMaxSize uint64 // 每次转发消息的最大自己数量

	r   *channelReactor
	sub *channelReactorSub

	mu sync.Mutex

	status   channelStatus // 频道状态
	role     channelRole   // 频道角色
	leaderId uint64        // 频道领导节点

	receiverTagKey atomic.String // 当前频道的接受者的tag key

	wklog.Log

	stepFnc func(*ChannelAction) error
	tickFnc func()

	// 解密
	payloadDecryptState readyState
	// 检查权限
	permissionCheckState readyState
	// 存储
	storageState readyState
	// 发送回执
	sendackState readyState
	// 投递
	deliveryState readyState
	// 转发
	forwardState readyState

	// 计时tick
	initTick int // 发起初始化的tick计时
	// payloadDecryptingTick  int // 发起解密的tick计时
	// permissionCheckingTick int // 发起权限检查的tick计时

	// storageTick int // 发起存储的tick计时
	forwardTick int // 发起转发的tick计时
	// deliveringTick int // 发起投递的tick计时
	// sendackingTick int // 发起发送回执的tick计时

	tagCheckTick int // tag检查的tick计时

	idleTick int // 频道闲置tick数

	opts *Options

	retryTickCount int // 多少次tick后重试
}

func newChannel(sub *channelReactorSub, channelId string, channelType uint8) *channel {
	key := wkutil.ChannelToKey(channelId, channelType)

	return &channel{
		key:            key,
		uniqueNo:       wkutil.GenUUID(),
		channelId:      channelId,
		channelType:    channelType,
		msgQueue:       newChannelMsgQueue(channelId),
		streams:        newStreamList(),
		storageMaxSize: 1024 * 1024 * 2,
		deliverMaxSize: 1024 * 1024 * 2,
		forwardMaxSize: 1024 * 1024 * 2,
		Log:            wklog.NewWKLog(fmt.Sprintf("channelHandler[%d][%s]", sub.r.opts.Cluster.NodeId, key)),
		r:              sub.r,
		sub:            sub,
		opts:           sub.r.opts,
		initTick:       sub.r.opts.Reactor.Channel.ProcessIntervalTick,
		retryTickCount: 20,
	}

}

func (c *channel) hasReady() bool {
	if !c.isInitialized() { // 是否初始化

		if c.initTick < c.opts.Reactor.Channel.ProcessIntervalTick {
			return false
		}

		return c.status != channelStatusInitializing
	}

	if c.hasPayloadUnDecrypt() || c.streams.hasPayloadUnDecrypt() { // 有未解密的消息
		return true
	}

	if c.role == channelRoleLeader { // 领导者
		if c.hasPermissionUnCheck() { // 是否有未检查权限的消息
			return true
		}
		if c.hasUnstorage() { // 是否有未存储的消息
			return true
		}

		if c.hasSendack() {
			return true
		}

		if c.hasUnDeliver() || c.streams.hasUnDeliver() { // 是否有未投递的消息
			return true
		}
	} else if c.role == channelRoleProxy { // 代理者
		if c.hasUnforward() || c.streams.hasUnforward() {
			return true
		}
	}
	return len(c.actions) > 0
}

func (c *channel) ready() ready {

	if !c.isInitialized() {
		if c.status == channelStatusInitializing {
			return ready{}
		}
		c.status = channelStatusInitializing
		c.initTick = 0
		c.exec(&ChannelAction{ActionType: ChannelActionInit})
	} else {

		// 解密消息
		if c.hasPayloadUnDecrypt() {
			c.payloadDecryptState.processing = true
			msgs := c.msgQueue.sliceWithSize(c.msgQueue.payloadDecryptingIndex+1, c.msgQueue.lastIndex+1, 1024*1024*2)
			if len(msgs) > 0 {
				c.exec(&ChannelAction{ActionType: ChannelActionPayloadDecrypt, Messages: msgs})
			}
		}

		// 流消息解密
		if c.streams.hasPayloadUnDecrypt() {
			msgs := c.streams.payloadUnDecryptMessages()
			if len(msgs) > 0 {
				c.exec(&ChannelAction{ActionType: ChannelActionStreamPayloadDecrypt, Messages: msgs})
			}
		}

		if c.role == channelRoleLeader {

			// 如果没有权限检查的则去检查权限
			if c.hasPermissionUnCheck() {
				c.permissionCheckState.processing = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.permissionCheckingIndex+1, c.msgQueue.payloadDecryptingIndex+1, 0)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionPermissionCheck, Messages: msgs})
				}
				// c.Info("permissionChecking...", zap.Uint64("permissionCheckingIndex", c.msgQueue.permissionCheckingIndex), zap.Uint64("payloadDecryptingIndex", c.msgQueue.payloadDecryptingIndex), zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))
			}

			// 如果有未存储的消息，则继续存储
			if c.hasUnstorage() {
				c.storageState.processing = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.storagingIndex+1, c.msgQueue.permissionCheckingIndex+1, c.storageMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionStorage, Messages: msgs})
				}
				// c.Info("storaging...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))

			}

			// 如果有未发送回执的消息
			if c.hasSendack() {
				c.sendackState.processing = true
				// TODO: 这里有个问题，如果投递消息完成后，消息已经被删除了，可能会导致ack发送失败，因为没了消息，虽然概率低，但是还是有可能的
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.sendackingIndex+1, c.msgQueue.storagingIndex+1, 0)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionSendack, Messages: msgs})
				}
			}

			// 投递消息
			if c.hasUnDeliver() {
				c.deliveryState.processing = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.deliveringIndex+1, c.msgQueue.storagingIndex+1, c.deliverMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionDeliver, Messages: msgs})
				}
				// c.Info("delivering...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))
			}

			// 投递流消息
			if c.streams.hasUnDeliver() {
				msgs := c.streams.unDeliverMessages()
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionStreamDeliver, Messages: msgs})
				}
			}

		} else if c.role == channelRoleProxy {
			// 转发消息
			if c.hasUnforward() {
				c.forwardState.processing = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.forwardingIndex+1, c.msgQueue.payloadDecryptingIndex+1, c.deliverMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionForward, LeaderId: c.leaderId, Messages: msgs})
				}
				// c.Info("forwarding...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))
			}

			// 转发流消息
			if c.streams.hasUnforward() {
				msgs := c.streams.unforwardMessages()
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionStreamForward, LeaderId: c.leaderId, Messages: msgs})
				}
			}
		}

	}

	actions := c.actions
	c.actions = nil
	return ready{
		actions: actions,
	}
}

func (c *channel) hasPayloadUnDecrypt() bool {
	if c.payloadDecryptState.processing {
		return false
	}

	return c.msgQueue.payloadDecryptingIndex < c.msgQueue.lastIndex
}

// 有未权限检查的消息
func (c *channel) hasPermissionUnCheck() bool {
	if c.permissionCheckState.processing {
		return false
	}

	return c.msgQueue.permissionCheckingIndex < c.msgQueue.payloadDecryptingIndex
}

// 有未存储的消息
func (c *channel) hasUnstorage() bool {
	if c.storageState.processing {
		return false
	}

	return c.msgQueue.storagingIndex < c.msgQueue.permissionCheckingIndex
}

// 有未发送回执的消息
func (c *channel) hasSendack() bool {
	if c.sendackState.processing {
		return false
	}

	return c.msgQueue.sendackingIndex < c.msgQueue.storagingIndex
}

// 有未投递的消息
func (c *channel) hasUnDeliver() bool {
	if c.deliveryState.processing {
		return false
	}

	return c.msgQueue.deliveringIndex < c.msgQueue.storagingIndex
}

// 有未转发的消息
func (c *channel) hasUnforward() bool {
	if c.forwardState.processing { // 在转发中
		return false
	}

	return c.msgQueue.forwardingIndex < c.msgQueue.payloadDecryptingIndex
}

// 是否已初始化
func (c *channel) isInitialized() bool {

	return c.status == channelStatusInitialized
}

func (c *channel) tick() {
	// c.storageTick++
	c.initTick++
	c.forwardTick++
	// c.deliveringTick++
	// c.permissionCheckingTick++
	// c.payloadDecryptingTick++
	// c.sendackingTick++

	c.idleTick++
	if c.idleTick >= c.opts.Reactor.Channel.DeadlineTick {
		c.idleTick = 0
		c.exec(&ChannelAction{ActionType: ChannelActionClose})
	}

	if c.payloadDecryptState.willRetry {
		c.payloadDecryptState.retryTick++
		if c.payloadDecryptState.retryTick >= c.retryTickCount {
			c.payloadDecryptState.willRetry = false
			c.payloadDecryptState.retryTick = 0
		}
	}

	if c.permissionCheckState.willRetry {
		c.permissionCheckState.retryTick++
		if c.permissionCheckState.retryTick >= c.retryTickCount {
			c.permissionCheckState.willRetry = false
			c.permissionCheckState.retryTick = 0
		}
	}

	if c.storageState.willRetry {
		c.storageState.retryTick++
		if c.storageState.retryTick >= c.retryTickCount {
			c.storageState.willRetry = false
			c.storageState.retryTick = 0
		}
	}

	if c.sendackState.willRetry {
		c.sendackState.retryTick++
		if c.sendackState.retryTick >= c.retryTickCount {
			c.sendackState.willRetry = false
			c.sendackState.retryTick = 0
		}
	}

	if c.deliveryState.willRetry {
		c.deliveryState.retryTick++
		if c.deliveryState.retryTick >= c.retryTickCount {
			c.deliveryState.willRetry = false
			c.deliveryState.retryTick = 0
		}
	}

	if c.forwardState.willRetry {
		c.forwardState.retryTick++
		if c.forwardState.retryTick >= c.retryTickCount {
			c.forwardState.willRetry = false
			c.forwardState.retryTick = 0
		}
	}

	// if c.payloadDecrypting && c.payloadDecryptingTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.payloadDecrypting = false
	// 	c.payloadDecryptingTick = 0
	// }
	// if c.permissionChecking && c.permissionCheckingTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.permissionChecking = false
	// 	c.permissionCheckingTick = 0
	// }
	// if c.storaging && c.storageTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.storaging = false
	// 	c.storageTick = 0
	// }
	// if c.sendacking && c.sendackingTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.sendacking = false
	// 	c.sendackingTick = 0
	// }
	// if c.delivering && c.deliveringTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.delivering = false
	// 	c.deliveringTick = 0
	// }
	// if c.forwarding && c.forwardTick > c.opts.Reactor.Channel.ProcessIntervalTick {
	// 	c.forwarding = false
	// 	c.forwardTick = 0
	// }

	if c.tickFnc != nil {
		c.tickFnc()
	}

	c.streams.tick()

}

func (c *channel) tickLeader() {
	c.tagCheckTick++
	if c.tagCheckTick >= c.opts.Reactor.Channel.TagCheckIntervalTick {
		c.tagCheckTick = 0
		if c.receiverTagKey.Load() != "" {
			c.exec(&ChannelAction{ActionType: ChannelActionCheckTag})
		}
	}
}

func (c *channel) tickProxy() {

}

func (c *channel) proposeSend(messageId int64, fromUid string, fromDeviceId string, fromConnId int64, fromNodeId uint64, isEncrypt bool, sendPacket *wkproto.SendPacket) error {

	message := ReactorChannelMessage{
		FromConnId:   fromConnId,
		FromUid:      fromUid,
		FromDeviceId: fromDeviceId,
		FromNodeId:   fromNodeId,
		SendPacket:   sendPacket,
		MessageId:    messageId,
		IsEncrypt:    isEncrypt,
		ReasonCode:   wkproto.ReasonSuccess, // 初始状态为成功
	}

	c.sub.step(c, &ChannelAction{
		UniqueNo:   c.uniqueNo,
		ActionType: ChannelActionSend,
		Messages:   []ReactorChannelMessage{message},
	})

	return nil
}

func (c *channel) becomeLeader() {
	c.resetIndex()
	c.leaderId = 0
	c.role = channelRoleLeader
	c.stepFnc = c.stepLeader
	c.tickFnc = c.tickLeader
	c.Info("become logic leader")

}

func (c *channel) becomeProxy(leaderId uint64) {
	c.resetIndex()
	c.role = channelRoleProxy
	c.leaderId = leaderId
	c.stepFnc = c.stepProxy
	c.tickFnc = c.tickProxy
	c.Info("become logic proxy", zap.Uint64("leaderId", c.leaderId))
}

func (c *channel) resetIndex() {
	c.msgQueue.resetIndex()

	// 释放掉之前的tag
	if c.receiverTagKey.Load() != "" {
		c.r.s.tagManager.releaseReceiverTagNow(c.receiverTagKey.Load())
		c.receiverTagKey.Store("")
	}

	c.payloadDecryptState = readyState{}
	c.permissionCheckState = readyState{}
	c.storageState = readyState{}
	c.sendackState = readyState{}
	c.deliveryState = readyState{}
	c.forwardState = readyState{}

	c.idleTick = 0

	c.initTick = 0
	// c.storageTick = 0
	// c.forwardTick = 0
	// c.deliveringTick = 0
	// c.permissionCheckingTick = 0
	// c.payloadDecryptingTick = 0
	// c.sendackingTick = 0

}

// func (c *channel) advance() {
// 	c.sub.advance()
// }

// // 是否是缓存中的订阅者
// func (c *channel) isCacheSubscriber(uid string) bool {
// 	_, ok := c.cacheSubscribers[uid]
// 	return ok
// }

// // 设置为缓存订阅者
// func (c *channel) setCacheSubscriber(uid string) {
// 	c.cacheSubscribers[uid] = struct{}{}
// }

type ready struct {
	actions []*ChannelAction
}

// makeReceiverTag 创建接收者标签
// 该方法用于为频道创建一个接收者标签，用于标识频道的订阅者及其所在的节点
// 返回创建的标签和可能的错误
func (c *channel) makeReceiverTag() (*tag, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Debug("makeReceiverTag", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))

	var (
		err         error
		subscribers []string
	)

	// 根据频道类型获取订阅者列表
	if c.channelType == wkproto.ChannelTypePerson {
		if c.r.s.opts.IsFakeChannel(c.channelId) { // 处理假个人频道
			orgFakeChannelId := c.channelId
			if c.r.s.opts.IsCmdChannel(c.channelId) {
				// 处理命令频道
				orgFakeChannelId = c.r.opts.CmdChannelConvertOrginalChannel(c.channelId)
			}
			// 处理普通假个人频道
			u1, u2 := GetFromUIDAndToUIDWith(orgFakeChannelId)
			if u1 != c.r.opts.SystemUID {
				subscribers = append(subscribers, u1)
			}
			if u2 != c.r.opts.SystemUID {
				subscribers = append(subscribers, u2)
			}
		}
	} else if c.channelType == wkproto.ChannelTypeTemp { // 临时频道
		subscribers = c.getTmpSubscribers()
	} else {

		// 处理非个人频道
		fakeChannelId := c.channelId
		if c.r.s.opts.IsCmdChannel(c.channelId) {
			fakeChannelId = c.r.opts.CmdChannelConvertOrginalChannel(c.channelId) // 将cmd频道id还原成对应的频道id
		}

		// 请求频道的订阅者
		subscribers, err = c.requestSubscribers(fakeChannelId, c.channelType)
		if err != nil {
			return nil, err
		}

		// 如果是客服频道，获取访客的uid作为订阅者
		if c.channelType == wkproto.ChannelTypeCustomerService {
			visitorID, _ := c.opts.GetCustomerServiceVisitorUID(fakeChannelId) // 获取访客ID
			if strings.TrimSpace(visitorID) != "" {
				subscribers = append(subscribers, visitorID)
			}
		}
	}

	// 将订阅者按所在节点分组
	var nodeUserList = make([]*nodeUsers, 0, 20)
	for _, subscriber := range subscribers {
		leaderInfo, err := c.r.s.cluster.SlotLeaderOfChannel(subscriber, wkproto.ChannelTypePerson)
		if err != nil {
			c.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", subscriber), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			return nil, err
		}
		exist := false
		for _, nodeUser := range nodeUserList {
			if nodeUser.nodeId == leaderInfo.Id {
				nodeUser.uids = append(nodeUser.uids, subscriber)
				exist = true
				break
			}
		}
		if !exist {
			nodeUserList = append(nodeUserList, &nodeUsers{
				nodeId: leaderInfo.Id,
				uids:   []string{subscriber},
			})
		}
	}

	// 释放旧的接收者标签（如果存在）
	if c.receiverTagKey.Load() != "" {
		c.r.s.tagManager.releaseReceiverTagNow(c.receiverTagKey.Load())
	}

	// 创建新的接收者标签
	receiverTagKey := wkutil.GenUUID()
	newTag := c.r.s.tagManager.addOrUpdateReceiverTag(receiverTagKey, nodeUserList, c.channelId, c.channelType)
	c.receiverTagKey.Store(receiverTagKey)

	return newTag, nil
}

// requestSubscribers 请求订阅者
func (c *channel) requestSubscribers(channelId string, channelType uint8) ([]string, error) {

	leaderNode, err := c.r.s.cluster.LeaderOfChannelForRead(channelId, channelType)
	if err != nil {
		return nil, err
	}
	if leaderNode == nil {
		return nil, errors.New("requestSubscribers: channel leader is nil")
	}

	if leaderNode.Id == c.r.s.opts.Cluster.NodeId {
		// 如果是本节点，则直接获取订阅者
		members, err := c.r.s.store.GetSubscribers(channelId, channelType)
		if err != nil {
			return nil, err
		}
		var subscribers []string
		for _, member := range members {
			subscribers = append(subscribers, member.Uid)
		}
		return subscribers, nil

	}

	timeoutCtx, cancel := context.WithTimeout(c.r.s.ctx, time.Second*5)
	defer cancel()

	req := &subscriberGetReq{
		ChannelId:   channelId,
		ChannelType: channelType,
	}
	data := req.Marshal()

	resp, err := c.r.s.cluster.RequestWithContext(timeoutCtx, leaderNode.Id, "/wk/getSubscribers", data)
	if err != nil {
		return nil, err
	}

	if resp.Status != proto.StatusOK {
		c.Error("requestSubscribers: response status code is not ok", zap.Int("status", int(resp.Status)), zap.String("body", string(resp.Body)))
		return nil, fmt.Errorf("requestSubscribers: response status code is %d", resp.Status)
	}

	subResp := subscriberGetResp{}
	err = subResp.Unmarshal(resp.Body)
	if err != nil {
		return nil, err
	}
	return subResp, nil

}

func (c *channel) setTmpSubscribers(subscribers []string) {
	c.tmpSubscribersLock.Lock()
	defer c.tmpSubscribersLock.Unlock()
	c.tmpSubscribers = subscribers
}

func (c *channel) getTmpSubscribers() []string {
	c.tmpSubscribersLock.RLock()
	defer c.tmpSubscribersLock.RUnlock()
	subs := make([]string, len(c.tmpSubscribers))
	copy(subs, c.tmpSubscribers)
	return subs
}
