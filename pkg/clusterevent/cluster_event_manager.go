package clusterevent

import (
	"fmt"
	"os"
	"path"
	"sort"
	"sync"

	"github.com/WuKongIM/WuKongIM/pkg/clusterevent/pb"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	"github.com/lni/goutils/syncutil"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type ClusterEventManager struct {
	watchCh chan ClusterEvent
	stopper *syncutil.Stopper
	wklog.Log
	opts              *Options
	clusterconfigLock sync.RWMutex
	clusterconfig     *pb.Cluster
	nodeLeaderID      atomic.Uint64 // 节点领导者id

	othersNodeConfigVersionMapLock sync.RWMutex
	othersNodeConfigVersionMap     map[uint64]uint32 // 其他节点目前集群配置的版本

	slotIsInit atomic.Bool // slot是否初始化
}

func NewClusterEventManager(opts *Options) *ClusterEventManager {

	c := &ClusterEventManager{
		watchCh:                    make(chan ClusterEvent),
		stopper:                    syncutil.NewStopper(),
		Log:                        wklog.NewWKLog(fmt.Sprintf("ClusterEventManager[%d]", opts.NodeID)),
		opts:                       opts,
		othersNodeConfigVersionMap: make(map[uint64]uint32),
	}

	if opts.DataDir != "" {
		err := os.MkdirAll(opts.DataDir, os.ModePerm)
		if err != nil {
			c.Panic("Create data dir failed!", zap.String("dataDir", opts.DataDir))
		}
	}

	if c.existClusterConfig() {
		c.initClusterConfigFromFile()
	} else {
		c.createAndInitClusterConfig()
	}
	return c

}

func (c *ClusterEventManager) existClusterConfig() bool {
	clusterCfgPath := path.Join(c.opts.DataDir, c.opts.ClusterConfigName)
	_, err := os.Stat(clusterCfgPath)
	if err != nil {
		if os.IsExist(err) {
			return true
		}
	}
	return false
}

func (c *ClusterEventManager) initClusterConfigFromFile() {
	clusterCfgPath := path.Join(c.opts.DataDir, c.opts.ClusterConfigName)
	data, err := os.ReadFile(clusterCfgPath)
	if err != nil {
		c.Panic("Read cluster config file failed!", zap.Error(err))
	}
	c.clusterconfig = &pb.Cluster{}
	if len(data) > 0 {
		err = wkutil.ReadJSONByByte(data, c.clusterconfig)
		if err != nil {
			c.Panic("Unmarshal cluster config failed!", zap.Error(err))
		}
	}
}

func (c *ClusterEventManager) getClusterConfigPath() string {
	return path.Join(c.opts.DataDir, c.opts.ClusterConfigName)
}

func (c *ClusterEventManager) createAndInitClusterConfig() {
	clusterCfgPath := c.getClusterConfigPath()
	clusterCfgFile, err := os.OpenFile(clusterCfgPath, os.O_CREATE|os.O_RDWR, os.ModePerm)
	if err != nil {
		c.Panic("Create cluster config file failed!", zap.String("clusterCfgPath", clusterCfgPath))
	}
	defer clusterCfgFile.Close()

	c.clusterconfig = &pb.Cluster{
		Version:   0,
		SlotCount: c.opts.SlotCount,
	}

	for nodeID, addr := range c.opts.InitNodes {
		c.clusterconfig.Nodes = append(c.clusterconfig.Nodes, &pb.Node{
			Id:          nodeID,
			ClusterAddr: addr,
			Status:      pb.NodeStatus_NodeStatusWaitInit,
			Online:      true,
			AllowVote:   true,
			DataTerm:    1,
		})
	}
	sort.Sort(pb.NodeSlice(c.clusterconfig.Nodes))

	// allocSlotMap := allocSlotToNodes(c.clusterconfig.Nodes, c)

	// for _, v := range c.clusterconfig.Nodes {
	// 	v.Slots = allocSlotMap[v.Id].FormatSlots()
	// }

	_, err = clusterCfgFile.Write([]byte(wkutil.ToJSON(c.clusterconfig)))
	if err != nil {
		c.Panic("Write cluster config failed!", zap.Error(err))
	}
}

// 是否是节点领导者
func (c *ClusterEventManager) IsNodeLeader() bool {
	return c.nodeLeaderID.Load() != 0 && c.nodeLeaderID.Load() == c.opts.NodeID
}

func allocSlotToNodes(nodes []*pb.Node, c *ClusterEventManager) map[uint64]*wkutil.SlotBitMap {

	allocSlotMap := make(map[uint64]*wkutil.SlotBitMap)                     // 节点分配的槽位
	eachSlotCountOfNode := c.opts.SlotCount / uint32(len(c.opts.InitNodes)) // 每个节点分配的槽位数量
	// 剩余未分配的槽位数量
	remainSlotCount := c.opts.SlotCount % uint32(len(c.opts.InitNodes))

	var startSlot uint32 = 0
	for i := 0; i < len(c.clusterconfig.Nodes); i++ {
		slotBitMap := wkutil.NewSlotBitMap(c.opts.SlotCount)
		node := c.clusterconfig.Nodes[i]
		allocSlotMap[node.Id] = slotBitMap
		for j := 0; j < int(eachSlotCountOfNode); j++ {
			slotBitMap.SetSlot(startSlot, true)
			startSlot++
		}
		if remainSlotCount > 0 {
			slotBitMap.SetSlot(startSlot, true)
			startSlot++
			remainSlotCount--
		}
	}

	return allocSlotMap
}

func (c *ClusterEventManager) Start() error {
	c.stopper.RunWorker(func() {
		c.loop()
	})
	return nil
}

func (c *ClusterEventManager) Stop() {
	c.stopper.Stop()
}

// Watch 监听集群事件
func (c *ClusterEventManager) Watch() <-chan ClusterEvent {
	return c.watchCh
}

func (c *ClusterEventManager) GetClusterConfig() *pb.Cluster {
	c.clusterconfigLock.RLock()
	defer c.clusterconfigLock.RUnlock()
	return c.clusterconfig
}

func (c *ClusterEventManager) GetSlotCount() uint32 {
	c.clusterconfigLock.RLock()
	defer c.clusterconfigLock.RUnlock()
	return c.clusterconfig.SlotCount
}

func (c *ClusterEventManager) GetClusterConfigVersion() uint32 {
	c.clusterconfigLock.RLock()
	defer c.clusterconfigLock.RUnlock()
	return c.getClusterConfigVersion()
}

func (c *ClusterEventManager) getClusterConfigVersion() uint32 {
	if c.clusterconfig == nil {
		return 0
	}
	return c.clusterconfig.Version
}

// SetNodeLeaderID 设置节点领导者id
func (c *ClusterEventManager) SetNodeLeaderID(nodeID uint64) {
	c.nodeLeaderID.Store(nodeID)
}

// SetTerm 设置当前领导的任期
func (c *ClusterEventManager) SetTerm(term uint32) {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	c.clusterconfig.Term = term
	c.saveAndVersionInc()
}

func (c *ClusterEventManager) SetNodeConfigVersion(nodeID uint64, configVersion uint32) {
	c.othersNodeConfigVersionMapLock.Lock()
	defer c.othersNodeConfigVersionMapLock.Unlock()
	c.othersNodeConfigVersionMap[nodeID] = configVersion
}

// GetAllOnlineNode 获取所有在线节点
func (c *ClusterEventManager) GetAllOnlineNode() []*pb.Node {
	c.othersNodeConfigVersionMapLock.Lock()
	defer c.othersNodeConfigVersionMapLock.Unlock()
	nodes := c.clusterconfig.Nodes
	if len(nodes) == 0 {
		return nil
	}
	onlineNodes := make([]*pb.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Online {
			onlineNodes = append(onlineNodes, node)
		}
	}
	return onlineNodes
}

func (c *ClusterEventManager) GetAllowVoteNodes() []*pb.Node {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	nodes := c.clusterconfig.Nodes
	if len(nodes) == 0 {
		return nil
	}
	allowVoteNodes := make([]*pb.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.AllowVote {
			allowVoteNodes = append(allowVoteNodes, node)
		}
	}
	return allowVoteNodes
}

func (c *ClusterEventManager) GetNode(nodeID uint64) *pb.Node {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	for _, node := range c.clusterconfig.Nodes {
		if node.Id == nodeID {
			return node
		}
	}
	return nil
}

// NodeIsOnline 节点是否在线
func (c *ClusterEventManager) NodeIsOnline(nodeID uint64) bool {
	c.othersNodeConfigVersionMapLock.Lock()
	defer c.othersNodeConfigVersionMapLock.Unlock()
	nodes := c.clusterconfig.Nodes
	if len(nodes) == 0 {
		return false
	}
	for _, node := range nodes {
		if node.Id == nodeID {
			return node.Online
		}
	}
	return false
}

func (c *ClusterEventManager) GetDataTerm(nodeID uint64) uint32 {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	for _, node := range c.clusterconfig.Nodes {
		if node.Id == nodeID {
			return node.DataTerm
		}
	}
	return 0
}

func (c *ClusterEventManager) save() error {
	configPathTmp := c.getClusterConfigPath() + ".tmp"
	f, err := os.Create(configPathTmp)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(wkutil.ToJSON(c.clusterconfig))
	if err != nil {
		return err
	}
	return os.Rename(configPathTmp, c.getClusterConfigPath())
}

func (c *ClusterEventManager) SaveAndVersionInc() {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()

	c.saveAndVersionInc()
}

func (c *ClusterEventManager) saveAndVersionInc() {
	c.clusterconfig.Version++
	err := c.save()
	if err != nil {
		c.Warn("save clusterconfig failed", zap.Error(err))
	}
}

func (c *ClusterEventManager) UpdateClusterConfig(cfg *pb.Cluster) {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	c.clusterconfig = cfg
	err := c.save()
	if err != nil {
		c.Warn("save clusterconfig failed", zap.Error(err))
	}
}

// GetSlotLeaderID 获取槽位的领导者id
func (c *ClusterEventManager) GetSlotLeaderID(slotID uint32) uint64 {
	c.clusterconfigLock.RLock()
	defer c.clusterconfigLock.RUnlock()
	return c.getSlotLeaderID(slotID)
}

// GetSlotReplica 获取槽的副本节点
func (c *ClusterEventManager) GetSlotReplicas(slotID uint32) []uint64 {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	return c.getSlotReplicas(slotID)

}

func (c *ClusterEventManager) GetSlots() []*pb.Slot {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	return c.clusterconfig.Slots
}

func (c *ClusterEventManager) GetSlot(slotID uint32) *pb.Slot {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	for _, slot := range c.clusterconfig.Slots {
		if slot.Id == slotID {
			return slot
		}
	}
	return nil
}

func (c *ClusterEventManager) UpdateSlotLeaderNoSave(slotID uint32, leaderID uint64) {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	for _, slot := range c.clusterconfig.Slots {
		if slot.Id == slotID {
			slot.Leader = leaderID
			break
		}
	}
}

func (c *ClusterEventManager) SetSlotIsInit(v bool) {
	c.slotIsInit.Store(v)
}

func (c *ClusterEventManager) AddOrUpdateSlotNoSave(slot *pb.Slot) {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()

	exist := false
	for idx, st := range c.clusterconfig.Slots {
		if st.Id == slot.Id {
			c.clusterconfig.Slots[idx] = slot
			exist = true
		}
	}
	if !exist {
		c.clusterconfig.Slots = append(c.clusterconfig.Slots, slot)
	}
}

// SetNodeOnline 设置节点在线状态
func (c *ClusterEventManager) SetNodeOnline(nodeID uint64, online bool) {
	c.SetNodeOnlineNoSave(nodeID, online)
	c.SaveAndVersionInc()
}

func (c *ClusterEventManager) SetNodeOnlineNoSave(nodeID uint64, online bool) {
	for _, node := range c.clusterconfig.Nodes {
		if node.Id == nodeID {
			node.Online = online
			if !online {
				node.OfflineCount++
				node.DataTerm++
			}
			break
		}
	}
}

func (c *ClusterEventManager) UpdateNode(n *pb.Node) {
	c.clusterconfigLock.Lock()
	defer c.clusterconfigLock.Unlock()
	for _, node := range c.clusterconfig.Nodes {
		if node.Id == n.Id {
			node.ApiAddr = n.ApiAddr // 目前仅支持更新api地址，需要更新其他信息，写到这里
			break
		}
	}
	c.saveAndVersionInc()
}

func (c *ClusterEventManager) getSlotReplicas(slotID uint32) []uint64 {
	for _, slot := range c.clusterconfig.Slots {
		if slot.Id == slotID {
			return slot.GetReplicas()
		}
	}
	return nil
}

func (c *ClusterEventManager) getSlotLeaderID(slotID uint32) uint64 {
	for _, slot := range c.clusterconfig.Slots {
		if slot.Id == slotID {
			return slot.GetLeader()
		}
	}
	return 0
}