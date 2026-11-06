package registry

import (
	"testing"

	"github.com/hashicorp/consul/api"
)

func TestHealthyServiceHandler(t *testing.T) {
	watcher := newWatcher()
	serviceEntry := newServiceEntry(
		"node-name", "node-address", "service-name", "v1.0.0",
		[]*api.HealthCheck{
			newHealthCheck("node-name", "service-name", "passing"),
		},
	)

	watcher.serviceHandler(1234, []*api.ServiceEntry{serviceEntry})

	if len(watcher.services["service-name"][0].Nodes) != 1 {
		t.Errorf("Expected length of the service nodes to be 1")
	}
}

func TestHealthyServiceHandlerWithNativeServiceMeta(t *testing.T) {
	watcher := newWatcher()
	serviceEntry := newServiceEntry(
		"node-name", "node-address", "service-name", "v1.0.0",
		[]*api.HealthCheck{
			newHealthCheck("node-name", "service-name", "passing"),
		},
	)
	serviceEntry.Service.Meta = map[string]string{"sid": "10001"}

	watcher.serviceHandler(1234, []*api.ServiceEntry{serviceEntry})

	if got := watcher.services["service-name"][0].Nodes[0].Metadata["sid"]; got != "10001" {
		t.Errorf("Expected native meta sid to be `%s`, got `%s`.", "10001", got)
	}
}

func TestUnhealthyServiceHandler(t *testing.T) {
	watcher := newWatcher()
	serviceEntry := newServiceEntry(
		"node-name", "node-address", "service-name", "v1.0.0",
		[]*api.HealthCheck{
			newHealthCheck("node-name", "service-name", "critical"),
		},
	)

	watcher.serviceHandler(1234, []*api.ServiceEntry{serviceEntry})

	if len(watcher.services["service-name"][0].Nodes) != 0 {
		t.Errorf("Expected length of the service nodes to be 0")
	}
}

func TestUnhealthyNodeServiceHandler(t *testing.T) {
	watcher := newWatcher()
	serviceEntry := newServiceEntry(
		"node-name", "node-address", "service-name", "v1.0.0",
		[]*api.HealthCheck{
			newHealthCheck("node-name", "service-name", "passing"),
			newHealthCheck("node-name", "serfHealth", "critical"),
		},
	)

	watcher.serviceHandler(1234, []*api.ServiceEntry{serviceEntry})

	if len(watcher.services["service-name"][0].Nodes) != 0 {
		t.Errorf("Expected length of the service nodes to be 0")
	}
}

// TestServiceHandlerPreservesSnapshots 验证节点删除不修改旧快照或已发布事件。
func TestServiceHandlerPreservesSnapshots(t *testing.T) {
	watcher := newWatcher()
	a := newServiceEntry("a", "address-a", "game", "1", nil)
	a.Service.ID = "a"
	b := newServiceEntry("b", "address-b", "game", "1", nil)
	b.Service.ID = "b"
	watcher.serviceHandler(1, []*api.ServiceEntry{a, b})
	published := <-watcher.next
	previous := watcher.services["game"][0]

	watcher.serviceHandler(2, []*api.ServiceEntry{a})
	deletion := <-watcher.next
	update := <-watcher.next
	if len(previous.Nodes) != 2 || len(published.Service.Nodes) != 2 {
		t.Fatal("deleting b modified the previous [a,b] snapshot")
	}
	if deletion.Action != "delete" || len(deletion.Service.Nodes) != 1 || deletion.Service.Nodes[0].Id != "b" {
		t.Fatalf("unexpected deletion: %+v", deletion)
	}
	if update.Action != "update" || len(update.Service.Nodes) != 1 || update.Service.Nodes[0].Id != "a" {
		t.Fatalf("unexpected update: %+v", update)
	}
	// 消费者修改事件时，监听器自己的快照也必须保持完整。
	update.Service.Nodes[0].Id = "changed"
	if got := watcher.services["game"][0].Nodes[0].Id; got != "a" {
		t.Fatalf("event consumer changed watcher snapshot to %q", got)
	}
}

func newWatcher() *consulWatcher {
	return &consulWatcher{
		exit:     make(chan bool),
		next:     make(chan *Result, 10),
		services: make(map[string][]*Service),
	}
}

func newHealthCheck(node, name, status string) *api.HealthCheck {
	return &api.HealthCheck{
		Node:        node,
		Name:        name,
		Status:      status,
		ServiceName: name,
	}
}

func newServiceEntry(node, address, name, version string, checks []*api.HealthCheck) *api.ServiceEntry {
	return &api.ServiceEntry{
		Node: &api.Node{Node: node, Address: name},
		Service: &api.AgentService{
			Service: name,
			Address: address,
			Tags:    encodeVersion(version),
		},
		Checks: checks,
	}
}
