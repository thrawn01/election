package election_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mailgun/holster/v3/election"
	"github.com/mailgun/holster/v3/slice"
	"github.com/mailgun/holster/v3/testutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	cfg            *election.Config
	ErrConnRefused = errors.New("connection refused")
)

func init() {
	logrus.SetLevel(logrus.DebugLevel)
	cfg = &election.Config{
		NetworkTimeout:      time.Second,
		HeartBeatTimeout:    time.Second,
		LeaderQuorumTimeout: time.Second * 2,
		ElectionTimeout:     time.Second * 2,
	}
}

func createCluster(t *testing.T, c *TestCluster) {
	t.Helper()

	// Start with a known leader
	err := c.SpawnNode("n0", cfg)
	require.NoError(t, err)
	testutil.UntilPass(t, 10, time.Second, func(t testutil.TestingT) {
		status := c.GetClusterStatus()
		assert.Equal(t, ClusterStatus{
			"n0": "n0",
		}, status)
	})

	// Added nodes should become followers
	c.SpawnNode("n1", cfg)
	c.SpawnNode("n2", cfg)
	c.SpawnNode("n3", cfg)
	c.SpawnNode("n4", cfg)

	testutil.UntilPass(t, 10, time.Second, func(t testutil.TestingT) {
		status := c.GetClusterStatus()
		assert.Equal(t, ClusterStatus{
			"n0": "n0",
			"n1": "n0",
			"n2": "n0",
			"n3": "n0",
			"n4": "n0",
		}, status)
	})
}

func TestSimpleElection(t *testing.T) {
	c := NewTestCluster()
	createCluster(t, c)
	defer c.Close()

	c.Nodes["n0"].Node.Resign()

	// Wait until n0 is no longer leader
	testutil.UntilPass(t, 30, time.Second, func(t testutil.TestingT) {
		candidate := c.GetLeader()
		if !assert.NotNil(t, candidate) {
			return
		}
		assert.NotEqual(t, "n0", candidate.Leader())
	})

	for k, v := range c.Nodes {
		t.Logf("Node: %s Leader: %t\n", k, v.Node.IsLeader())
	}
}

func TestLeaderDisconnect(t *testing.T) {
	c := NewTestCluster()
	createCluster(t, c)
	defer c.Close()

	c.AddNetworkError("n0", ErrConnRefused)
	defer c.DelNetworkError("n0")

	// Should lose leadership
	testutil.UntilPass(t, 30, time.Second, func(t testutil.TestingT) {
		node := c.Nodes["n0"]
		if !assert.NotNil(t, node.Node) {
			return
		}
		assert.NotEqual(t, "n0", node.Node.Leader())
	})

	for k, v := range c.Nodes {
		t.Logf("Node: %s Leader: %t\n", k, v.Node.IsLeader())
	}
}

func TestFollowerDisconnect(t *testing.T) {
	c := NewTestCluster()
	createCluster(t, c)
	defer c.Close()

	c.AddNetworkError("n4", ErrConnRefused)
	defer c.DelNetworkError("n4")

	// Wait until n4 loses leader
	testutil.UntilPass(t, 10, time.Second, func(t testutil.TestingT) {
		status := c.GetClusterStatus()
		assert.NotEqual(t, "n0", status["n4"])
	})

	c.DelNetworkError("n4")

	// Follower should resume being a follower without forcing a new election.
	testutil.UntilPass(t, 30, time.Second, func(t testutil.TestingT) {
		status := c.GetClusterStatus()
		assert.Equal(t, "n0", status["n4"])
	})
}

func TestSplitBrain(t *testing.T) {
	c1 := NewTestCluster()
	createCluster(t, c1)
	defer c1.Close()

	c2 := NewTestCluster()

	// Now take 2 nodes from cluster 1 and put them in their own cluster.
	// This causes n0 to lose contact with n2-n4 and should update the member list
	// such that n0 only knows about n1.

	// Since n0 was leader previously, it should remain leader
	c2.Add("n0", c1.Remove("n0"))
	c2.Add("n1", c1.Remove("n1"))

	// Cluster 1 should elect a new leader
	testutil.UntilPass(t, 30, time.Second, func(t testutil.TestingT) {
		assert.NotNil(t, c1.GetLeader())
	})

	for k, v := range c1.Nodes {
		t.Logf("C1 Node: %s Leader: %t\n", k, v.Node.IsLeader())
	}

	// Cluster 2 should elect a new leader
	testutil.UntilPass(t, 30, time.Second, func(t testutil.TestingT) {
		assert.NotNil(t, c2.GetLeader())
	})

	for k, v := range c2.Nodes {
		t.Logf("C2 Node: %s Leader: %t\n", k, v.Node.IsLeader())
	}

	// Move the nodes in cluster2, back to the cluster1
	c1.Add("n0", c2.Remove("n0"))
	c1.Add("n1", c2.Remove("n1"))

	// The nodes should detect 2 leaders and start a new vote.
	testutil.UntilPass(t, 10, time.Second, func(t testutil.TestingT) {
		status := c1.GetClusterStatus()
		var leaders []string
		for _, v := range status {
			if slice.ContainsString(v, leaders, nil) {
				continue
			}
			leaders = append(leaders, v)
		}
		if !assert.NotNil(t, leaders) {
			return
		}
		assert.Equal(t, 1, len(leaders))
		assert.NotEmpty(t, leaders[0])
	})

	for k, v := range c1.Nodes {
		t.Logf("Node: %s Leader: %t\n", k, v.Node.IsLeader())
	}
}

func TestOmissionFaults(t *testing.T) {
	c1 := NewTestCluster()
	createCluster(t, c1)
	defer c1.Close()

	// Create an unstable cluster with n3 and n4 only able to contact n1 and n2 respectively.
	// The end result should be an omission fault of less than quorum.
	//
	// Diagram: lines indicate connectivity between nodes
	// (n0)-----(n1)----(n4)
	//   \       /
	//	  \     /
	//     \   /
	//      (n2)----(n3)
	//

	// n3 and n4 can't talk
	c1.AddPeerToPeerError("n3", "n4", ErrConnRefused)
	c1.AddPeerToPeerError("n4", "n3", ErrConnRefused)

	// Leader can't talk to n4
	c1.AddPeerToPeerError("n0", "n4", ErrConnRefused)
	c1.AddPeerToPeerError("n4", "n0", ErrConnRefused)

	// Leader can't talk to n3
	c1.AddPeerToPeerError("n0", "n3", ErrConnRefused)
	c1.AddPeerToPeerError("n3", "n0", ErrConnRefused)

	// n2 and n4 can't talk
	c1.AddPeerToPeerError("n2", "n4", ErrConnRefused)
	c1.AddPeerToPeerError("n4", "n2", ErrConnRefused)

	// n1 and n3 can't talk
	c1.AddPeerToPeerError("n1", "n3", ErrConnRefused)
	c1.AddPeerToPeerError("n3", "n1", ErrConnRefused)

	// Cluster should retain n0 as leader in the face on unstable cluster
	for i := 0; i < 12; i++ {
		leader := c1.GetLeader()
		require.NotNil(t, leader)
		require.Equal(t, leader.Leader(), "n0")
		time.Sleep(time.Millisecond * 400)
	}

	// Should retain leader once communication is restored
	c1.ClearErrors()

	for i := 0; i < 12; i++ {
		leader := c1.GetLeader()
		require.NotNil(t, leader)
		require.Equal(t, leader.Leader(), "n0")
		time.Sleep(time.Millisecond * 400)
	}
}
