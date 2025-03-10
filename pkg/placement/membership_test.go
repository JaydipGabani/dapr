/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

//nolint:forbidigo,nosnakecase
package placement

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/dapr/dapr/pkg/placement/raft"
	v1pb "github.com/dapr/dapr/pkg/proto/placement/v1"
)

func cleanupStates() {
	for k := range testRaftServer.FSM().State().Members() {
		testRaftServer.ApplyCommand(raft.MemberRemove, raft.DaprHostMember{
			Name: k,
		})
	}
}

func TestMembershipChangeWorker(t *testing.T) {
	var (
		serverAddress string
		testServer    *Service
	)

	setupEach := func(t *testing.T) context.CancelFunc {
		t.Helper()

		ctx, cancel := context.WithCancel(context.Background())
		var cancelServer context.CancelFunc
		serverAddress, testServer, cancelServer = newTestPlacementServer(t, testRaftServer)
		testServer.hasLeadership.Store(true)
		membershipStopCh := make(chan struct{})

		cleanupStates()
		assert.Equal(t, 0, len(testServer.raftNode.FSM().State().Members()))

		go func() {
			defer close(membershipStopCh)
			testServer.membershipChangeWorker(ctx)
		}()

		return func() {
			cancelServer()
			cancel()
			select {
			case <-membershipStopCh:
			case <-time.After(time.Second):
				t.Error("membershipChangeWorker did not stop in time")
			}
		}
	}

	arrangeFakeMembers := func(t *testing.T) {
		for i := 0; i < 3; i++ {
			testServer.membershipCh <- hostMemberChange{
				cmdType: raft.MemberUpsert,
				host: raft.DaprHostMember{
					Name:     fmt.Sprintf("127.0.0.1:%d", 50000+i),
					AppID:    fmt.Sprintf("TestAPPID%d", 50000+i),
					Entities: []string{"actorTypeOne", "actorTypeTwo"},
				},
			}
		}

		// wait until all host member change requests are flushed
		assert.Eventually(t, func() bool {
			return len(testServer.raftNode.FSM().State().Members()) == 3
		}, disseminateTimerInterval+100*time.Millisecond, 10*time.Millisecond)
	}

	t.Run("successful dissemination", func(t *testing.T) {
		teardown := setupEach(t)
		// arrange
		testServer.faultyHostDetectDuration.Store(int64(faultyHostDetectInitialDuration))

		conn, stream, err := newTestClient(serverAddress)
		assert.NoError(t, err)
		done := make(chan bool)
		go func() {
			for {
				placementOrder, streamErr := stream.Recv()
				require.NoError(t, streamErr)
				if placementOrder.Operation == "unlock" {
					done <- true
					return
				}
			}
		}()

		host := &v1pb.Host{
			Name:     "127.0.0.1:50100",
			Entities: []string{"DogActor", "CatActor"},
			Id:       "testAppID",
			Load:     1, // Not used yet
			// Port is redundant because Name should include port number
		}

		// act
		assert.NoError(t, stream.Send(host))

		select {
		case <-done:
		case <-time.After(disseminateTimerInterval + 100*time.Millisecond):
			t.Error("dissemination did not happen in time")
		}

		// ignore disseminateTimeout.
		testServer.disseminateNextTime.Store(0)

		testServer.streamConnPoolLock.RLock()
		nStreamConnPool := len(testServer.streamConnPool)
		testServer.streamConnPoolLock.RUnlock()
		require.Equal(t, 1, nStreamConnPool)
		assert.Eventually(t, func() bool {
			return nStreamConnPool == len(testServer.raftNode.FSM().State().Members())
		}, time.Second, 10*time.Millisecond, testServer.raftNode.FSM().State().Members())

		// wait until table dissemination.
		assert.Eventually(t, func() bool {
			return testServer.memberUpdateCount.Load() == 0 &&
				testServer.faultyHostDetectDuration.Load() == int64(faultyHostDetectDefaultDuration)
		}, disseminateTimerInterval*10, 10*time.Millisecond,
			"flushed all member updates and faultyHostDetectDuration must be faultyHostDetectDuration")

		conn.Close()

		teardown()
	})

	t.Run("faulty host detector", func(t *testing.T) {
		// arrange
		teardown := setupEach(t)
		arrangeFakeMembers(t)

		// faulty host detector removes all members if heartbeat does not happen
		assert.Eventually(t, func() bool {
			return len(testServer.raftNode.FSM().State().Members()) == 0
		}, faultyHostDetectInitialDuration+100*time.Millisecond, 10*time.Millisecond)

		teardown()
	})
}

func TestCleanupHeartBeats(t *testing.T) {
	_, testServer, cleanup := newTestPlacementServer(t, testRaftServer)
	testServer.hasLeadership.Store(true)
	maxClients := 3

	for i := 0; i < maxClients; i++ {
		testServer.lastHeartBeat.Store(fmt.Sprintf("10.0.0.%d:1001", i), time.Now().UnixNano())
	}

	getCount := func() int {
		cnt := 0
		testServer.lastHeartBeat.Range(func(k, v interface{}) bool {
			cnt++
			return true
		})

		return cnt
	}

	assert.Equal(t, maxClients, getCount())
	testServer.cleanupHeartbeats()
	assert.Equal(t, 0, getCount())
	cleanup()
}

func TestPerformTableUpdate(t *testing.T) {
	const testClients = 10
	serverAddress, testServer, cleanup := newTestPlacementServer(t, testRaftServer)
	testServer.hasLeadership.Store(true)

	// arrange
	clientConns := []*grpc.ClientConn{}
	clientStreams := []v1pb.Placement_ReportDaprStatusClient{}
	clientRecvDataLock := &sync.RWMutex{}
	clientRecvData := []map[string]int64{}
	clientUpToDateCh := make(chan struct{}, testClients)

	for i := 0; i < testClients; i++ {
		conn, stream, err := newTestClient(serverAddress)
		assert.NoError(t, err)
		clientConns = append(clientConns, conn)
		clientStreams = append(clientStreams, stream)
		clientRecvData = append(clientRecvData, map[string]int64{})

		go func(clientID int, clientStream v1pb.Placement_ReportDaprStatusClient) {
			upToDate := false
			for {
				placementOrder, streamErr := clientStream.Recv()
				if streamErr != nil {
					return
				}
				if placementOrder != nil {
					clientRecvDataLock.Lock()
					clientRecvData[clientID][placementOrder.Operation] = time.Now().UnixNano()
					clientRecvDataLock.Unlock()
					// Check if the table is up to date.
					if placementOrder.Operation == "update" {
						if placementOrder.Tables != nil {
							upToDate = true
							for _, entries := range placementOrder.Tables.Entries {
								// Check if all clients are in load map.
								if len(entries.LoadMap) != testClients {
									upToDate = false
								}
							}
						}
					}
					if placementOrder.Operation == "unlock" {
						if upToDate {
							clientUpToDateCh <- struct{}{}
							return
						}
					}
				}
			}
		}(i, stream)
	}

	// act
	for i := 0; i < testClients; i++ {
		host := &v1pb.Host{
			Name:     fmt.Sprintf("127.0.0.1:5010%d", i),
			Entities: []string{"DogActor", "CatActor", fmt.Sprintf("127.0.0.1:5010%d", i)},
			Id:       "testAppID",
			Load:     1, // Not used yet
			// Port is redundant because Name should include port number
		}

		// act
		assert.NoError(t, clientStreams[i].Send(host))
	}

	// Wait until clientStreams[clientID].Recv() in client go routine received new table
	waitCnt := testClients
	timeoutC := time.After(time.Second * 10)
	for {
		end := false
		select {
		case <-clientUpToDateCh:
			waitCnt--
			if waitCnt == 0 {
				end = true
			}
		case <-timeoutC:
			end = true
		}
		if end {
			break
		}
	}

	// Call performTableUpdate directly, not by MembershipChangeWorker loop.
	testServer.streamConnPoolLock.RLock()
	streamConnPool := make([]placementGRPCStream, len(testServer.streamConnPool))
	copy(streamConnPool, testServer.streamConnPool)
	testServer.streamConnPoolLock.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, testServer.performTablesUpdate(ctx, streamConnPool, nil))

	// assert
	for i := 0; i < testClients; i++ {
		clientRecvDataLock.RLock()
		lockTime, ok := clientRecvData[i]["lock"]
		clientRecvDataLock.RUnlock()
		assert.True(t, ok)
		clientRecvDataLock.RLock()
		updateTime, ok := clientRecvData[i]["update"]
		clientRecvDataLock.RUnlock()
		assert.True(t, ok)
		clientRecvDataLock.RLock()
		unlockTime, ok := clientRecvData[i]["unlock"]
		clientRecvDataLock.RUnlock()
		assert.True(t, ok)

		// check if lock, update, and unlock operations are received in the right order.
		assert.True(t, lockTime <= updateTime && updateTime <= unlockTime)
	}

	// clean up resources
	for i := 0; i < testClients; i++ {
		clientStreams[i].CloseSend()
		clientConns[i].Close()
	}

	cleanup()
}

func PerformTableUpdateCostTime(t *testing.T) (wastedTime int64) {
	t.Helper()

	const testClients = 100
	serverAddress, testServer, cleanup := newTestPlacementServer(t, testRaftServer)
	testServer.hasLeadership.Store(true)
	var (
		overArrLock sync.Mutex
		overArr     [testClients]int64
		// arrange.
		clientConns   []*grpc.ClientConn
		clientStreams []v1pb.Placement_ReportDaprStatusClient
		wg            sync.WaitGroup
	)
	startFlag := atomic.Bool{}
	startFlag.Store(false)
	wg.Add(testClients)
	for i := 0; i < testClients; i++ {
		conn, stream, err := newTestClient(serverAddress)
		if err != nil {
			t.Fatal(err)
		}
		clientConns = append(clientConns, conn)
		clientStreams = append(clientStreams, stream)
		go func(clientID int, clientStream v1pb.Placement_ReportDaprStatusClient) {
			defer wg.Done()
			var start time.Time
			for {
				placementOrder, streamErr := clientStream.Recv()
				if streamErr != nil {
					return
				}
				if placementOrder != nil {
					if placementOrder.Operation == "lock" {
						if startFlag.Load() && placementOrder.Tables != nil && placementOrder.Tables.Version == "demo" {
							if clientID == 1 {
								fmt.Println("client 1 lock", time.Now())
							}
							start = time.Now()
						}
					}
					if placementOrder.Operation == "update" {
						continue
					}
					if placementOrder.Operation == "unlock" {
						if startFlag.Load() && placementOrder.Tables != nil && placementOrder.Tables.Version == "demo" {
							if clientID == 1 {
								fmt.Println("client 1 unlock", time.Now())
							}
							overArrLock.Lock()
							overArr[clientID] = time.Since(start).Milliseconds()
							overArrLock.Unlock()
						}
					}
				}
			}
		}(i, stream)
	}

	// register
	for i := 0; i < testClients; i++ {
		host := &v1pb.Host{
			Name:     fmt.Sprintf("127.0.0.1:5010%d", i),
			Entities: []string{"DogActor", "CatActor"},
			Id:       "testAppID",
			Load:     1, // Not used yet.
		}
		require.NoError(t, clientStreams[i].Send(host))
	}
	// Wait until clientStreams[clientID].Recv() in client go routine received new table.
	require.Eventually(t, func() bool {
		testServer.streamConnPoolLock.RLock()
		defer testServer.streamConnPoolLock.RUnlock()
		return len(testServer.streamConnPool) == testClients
	}, time.Second, time.Millisecond*10)

	testServer.streamConnPoolLock.RLock()
	streamConnPool := make([]placementGRPCStream, len(testServer.streamConnPool))
	copy(streamConnPool, testServer.streamConnPool)
	testServer.streamConnPoolLock.RUnlock()
	startFlag.Store(true)
	mockMessage := &v1pb.PlacementTables{Version: "demo"}

	for _, host := range streamConnPool {
		require.NoError(t, testServer.disseminateOperation(context.Background(), []placementGRPCStream{host}, "lock", mockMessage))
		require.NoError(t, testServer.disseminateOperation(context.Background(), []placementGRPCStream{host}, "update", mockMessage))
		require.NoError(t, testServer.disseminateOperation(context.Background(), []placementGRPCStream{host}, "unlock", mockMessage))
	}
	// fmt.Println("start lock ", time.Now())
	// testServer.disseminateOperation(streamConnPool, "lock", mockMessage)
	// fmt.Println("all lock  ", time.Now())
	// testServer.disseminateOperation(streamConnPool, "update", mockMessage)
	// fmt.Println("all update", time.Now())
	// testServer.disseminateOperation(streamConnPool, "unlock", mockMessage)
	// fmt.Println("all unlock", time.Now())
	startFlag.Store(false)
	var max int64
	overArrLock.Lock()
	for _, cost := range &overArr {
		if cost > max {
			max = cost
		}
	}
	overArrLock.Unlock()
	// clean up resources.
	for i := 0; i < testClients; i++ {
		require.NoError(t, clientConns[i].Close())
	}
	cleanup()
	return max
}

func TestPerformTableUpdatePerf(t *testing.T) {
	for i := 0; i < 3; i++ {
		fmt.Println("max cost time(ms)", PerformTableUpdateCostTime(t))
	}
}
