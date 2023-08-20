package transporter_test

import (
	"sync"
	"testing"

	"github.com/WuKongIM/WuKongIM/pkg/wraft/transporter"
	"github.com/stretchr/testify/assert"
)

func TestTransporterAuth(t *testing.T) {
	var wait sync.WaitGroup
	wait.Add(1)
	recvChan := make(chan transporter.Ready)
	tran := transporter.New(1, "tcp://0.0.0.0:0", recvChan, transporter.WithToken("1234"))
	err := tran.Start()
	assert.NoError(t, err)

	go func() {
		for ready := range recvChan {
			if string(ready.Req.Param) == "hello" {
				wait.Done()
			}
		}
	}()

	defer tran.Stop()

	cli := transporter.NewNodeClient(2, tran.Addr().String(), "1234", nil)
	err = cli.Connect()
	assert.NoError(t, err)

	req := &transporter.CMDReq{
		Param: []byte("hello"),
	}
	data, _ := req.Marshal()
	err = cli.Send(data)
	assert.NoError(t, err)

	wait.Wait()

}