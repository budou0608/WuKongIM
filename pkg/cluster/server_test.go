package cluster_test

import (
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/stretchr/testify/assert"
)

// 测试服务的开启和停止
func TestServerStartAndStop(t *testing.T) {
	// err := failpoint.Enable("github.com/WuKongIM/WuKongIM/pkg/cluster/mock-io-error", "return(\"disk error\")")
	// assert.NoError(t, err)
	s := cluster.NewServer(1, cluster.WithDataDir(os.TempDir()), cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithInitNodes(map[uint64]string{1: "127.0.0.1:10001"}))
	err := s.Start()
	assert.NoError(t, err)
	defer s.Stop()

}

// 测试节点领导选举
func TestServerWaitLeader(t *testing.T) {
	dataDir1 := path.Join(os.TempDir(), "cluster", "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
	}
	fmt.Println("dataDir1--->", dataDir1)
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	dataDir2 := path.Join(os.TempDir(), "cluster", "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	s2.MustWaitLeader(time.Second * 20)

}

// 测试槽的领导选举（两个节点）
func TestServerSlotLeaderElectionForTwo(t *testing.T) {
	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
	}
	var slotCount uint32 = 10
	fmt.Println("dataDir1--->", rootDir)
	defer os.RemoveAll(rootDir)
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	s2.BecomeLeader() // s2设置为领导

	s2.FakeSetNodeOnline(1, false) // 设置节点1下线

	s1.MustWaitAllSlotLeaderIs(2, time.Second*20) // 等待所有的slot的领导都为2

}

// 测试槽的领导选举（三个节点）
func TestServerSlotLeaderElectionForTree(t *testing.T) {
	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
		3: "127.0.0.1:10003",
	}
	var slotCount uint32 = 256
	fmt.Println("dataDir1--->", dataDir1)
	// defer os.RemoveAll(rootDir)
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	dataDir3 := path.Join(rootDir, "3")
	s3 := cluster.NewServer(3, cluster.WithListenAddr("127.0.0.1:10003"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir3))
	err = s3.Start()
	assert.NoError(t, err)
	defer s3.Stop()

	s1.BecomeLeader()                             // s1成为领导
	s1.FakeSetNodeOnline(2, false)                // 将节点2下线
	s3.MustWaitSlotLeaderNotIs(2, time.Second*20) // 等待所有的slot的领导都不包含2

}

// 测试提案槽数据
func TestServerSlotPropse(t *testing.T) {

	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
		3: "127.0.0.1:10003",
	}
	var slotCount uint32 = 256
	fmt.Println("dataDir1--->", dataDir1)
	defer os.RemoveAll(rootDir)
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	s1.BecomeLeader() // s1成为领导

	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	dataDir3 := path.Join(rootDir, "3")
	s3 := cluster.NewServer(3, cluster.WithListenAddr("127.0.0.1:10003"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir3))
	err = s3.Start()
	assert.NoError(t, err)
	defer s3.Stop()

	s3.MustWaitLeader(time.Second * 20)
	s2.MustWaitLeader(time.Second * 20)

	time.Sleep(time.Millisecond * 200)

	err = s1.ProposeToSlot(1, []byte("hello"))
	assert.NoError(t, err)

	time.Sleep(time.Millisecond * 200)

	// 验证s1的数据
	logs, err := s1.GetSlotLogs(1, 1, 1)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(logs))
	assert.Equal(t, uint64(1), logs[0].Index)
	assert.Equal(t, []byte("hello"), logs[0].Data)

	// 验证s2的数据
	logs, err = s2.GetSlotLogs(1, 1, 1)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(logs))
	assert.Equal(t, uint64(1), logs[0].Index)
	assert.Equal(t, []byte("hello"), logs[0].Data)

	// 验证s3的数据
	logs, err = s3.GetSlotLogs(1, 1, 1)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(logs))
	assert.Equal(t, uint64(1), logs[0].Index)
	assert.Equal(t, []byte("hello"), logs[0].Data)

	fmt.Println("stop...................")

}

// 测试提案频道集群数据
func TestServerProposeChanneClusterlInfo(t *testing.T) {
	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
	}
	var slotCount uint32 = 10
	fmt.Println("dataDir1-2-->", rootDir)
	defer os.RemoveAll(rootDir)
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	s2.BecomeLeader() // s2设置为领导

	s1.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导
	s2.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导

	channelID := "test"
	var channelType uint8 = 2

	err = s2.ProposeChannelClusterInfoToSlot(&cluster.ChannelClusterInfo{
		ChannelID:   channelID,
		ChannelType: channelType,
		LeaderID:    1,
		Replicas:    []uint64{2, 3},
	})
	assert.NoError(t, err)

	time.Sleep(time.Millisecond * 100)

	// 验证s1的数据
	channelInfo, err := s1.GetChannelClusterInfo(channelID, channelType)
	assert.NoError(t, err)

	assert.Equal(t, channelID, channelInfo.ChannelID)
	assert.Equal(t, channelType, channelInfo.ChannelType)
	assert.Equal(t, uint64(1), channelInfo.LeaderID)
	assert.Equal(t, []uint64{2, 3}, channelInfo.Replicas)

	// 验证s2的数据
	channelInfo, err = s2.GetChannelClusterInfo(channelID, channelType)
	assert.NoError(t, err)

	assert.Equal(t, channelID, channelInfo.ChannelID)
	assert.Equal(t, channelType, channelInfo.ChannelType)
	assert.Equal(t, uint64(1), channelInfo.LeaderID)
	assert.Equal(t, []uint64{2, 3}, channelInfo.Replicas)

}

// 测试提案频道数据
func TestServerProposeDataToChannel(t *testing.T) {
	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
	}
	var slotCount uint32 = 10
	fmt.Println("dataDir1-2-->", rootDir)
	// defer os.RemoveAll(rootDir)
	msgLogStorage1 := newTestMessageLogStorage()
	s1 := cluster.NewServer(1, cluster.WithListenAddr("127.0.0.1:10001"), cluster.WithMessageLogStorage(msgLogStorage1), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir1))
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	msgLogStorage2 := newTestMessageLogStorage()
	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(2, cluster.WithListenAddr("127.0.0.1:10002"), cluster.WithMessageLogStorage(msgLogStorage2), cluster.WithSlotCount(slotCount), cluster.WithHeartbeat(time.Millisecond*100), cluster.WithInitNodes(initNodes), cluster.WithDataDir(dataDir2))
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	s1.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导
	s2.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导

	channelID := "test"
	var channelType uint8 = 2

	err = s2.ProposeMetaToChannel(channelID, channelType, []byte("hello"))
	assert.NoError(t, err)

	time.Sleep(time.Millisecond * 100)

	// 验证s1的数据
	logs, err := s1.GetChannelLogs(channelID, channelType, 1, 1)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(logs))
	assert.Equal(t, []byte("hello"), logs[0].Data)

	// 验证s2的数据
	logs, err = s2.GetChannelLogs(channelID, channelType, 1, 1)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(logs))
	assert.Equal(t, []byte("hello"), logs[0].Data)

	fmt.Println("stop.............")
}

// 测试提案消息到频道
func TestServerProposeMessageToChannel(t *testing.T) {
	rootDir := path.Join(os.TempDir(), "cluster")
	dataDir1 := path.Join(rootDir, "1")

	initNodes := map[uint64]string{
		1: "127.0.0.1:10001",
		2: "127.0.0.1:10002",
	}
	var slotCount uint32 = 10
	fmt.Println("dataDir1-2-->", rootDir)

	msgLogStorage1 := newTestMessageLogStorage()

	// defer os.RemoveAll(rootDir)
	s1 := cluster.NewServer(
		1,
		cluster.WithListenAddr("127.0.0.1:10001"),
		cluster.WithSlotCount(slotCount),
		cluster.WithHeartbeat(time.Millisecond*100),
		cluster.WithInitNodes(initNodes),
		cluster.WithDataDir(dataDir1),
		cluster.WithMessageLogStorage(msgLogStorage1),
	)
	err := s1.Start()
	assert.NoError(t, err)
	defer s1.Stop()

	msgLogStorage2 := newTestMessageLogStorage()
	dataDir2 := path.Join(rootDir, "2")
	s2 := cluster.NewServer(
		2,
		cluster.WithListenAddr("127.0.0.1:10002"),
		cluster.WithSlotCount(slotCount),
		cluster.WithHeartbeat(time.Millisecond*100),
		cluster.WithInitNodes(initNodes),
		cluster.WithDataDir(dataDir2),
		cluster.WithMessageLogStorage(msgLogStorage2),
	)
	err = s2.Start()
	assert.NoError(t, err)
	defer s2.Stop()

	s1.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导
	s2.MustWaitAllSlotLeaderReady(time.Second * 20) // 等待所有slot都选出领导

	channelID := "test"
	var channelType uint8 = 2

	_, err = s2.ProposeMessageToChannel(channelID, channelType, []byte("hello"))
	assert.NoError(t, err)

	time.Sleep(time.Millisecond * 100)

	logs, err := msgLogStorage1.GetLogs(cluster.GetChannelKey(channelID, channelType), 0, 10)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(logs))
	assert.Equal(t, []byte("hello"), logs[0].Data)

}

type testMessageLogStorage struct {
	cacheMap map[string][]replica.Log
}

func newTestMessageLogStorage() *testMessageLogStorage {
	return &testMessageLogStorage{
		cacheMap: make(map[string][]replica.Log),
	}
}

// AppendLog 追加日志
func (t *testMessageLogStorage) AppendLog(shardNo string, log replica.Log) error {
	t.cacheMap[shardNo] = append(t.cacheMap[shardNo], log)
	return nil
}

// 获取日志
func (t *testMessageLogStorage) GetLogs(shardNo string, startLogIndex uint64, limit uint32) ([]replica.Log, error) {

	return t.cacheMap[shardNo][startLogIndex:], nil
}

// 最后一条日志的索引
func (t *testMessageLogStorage) LastIndex(shardNo string) (uint64, error) {
	if t.cacheMap[shardNo] == nil {
		return 0, nil
	}
	return uint64(len(t.cacheMap[shardNo])), nil
}

// 获取第一条日志的索引
func (t *testMessageLogStorage) FirstIndex(shardNo string) (uint64, error) {
	return 0, nil
}

// 设置成功被状态机应用的日志索引
func (t *testMessageLogStorage) SetAppliedIndex(shardNo string, index uint64) error {
	return nil
}

func (t *testMessageLogStorage) LastIndexAndAppendTime(shardNo string) (uint64, uint64, error) {
	return 0, 0, nil
}