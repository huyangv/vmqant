package registry

import (
	"maps"
	"slices"
)

// Service Service
type Service struct {
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Metadata  map[string]string `json:"metadata"`
	Endpoints []*Endpoint       `json:"endpoints"`
	Nodes     []*Node           `json:"nodes"`
}

// Clone 复制服务及其可修改字段，隔离监听快照、缓存和调用方。
func (s *Service) Clone() *Service {
	if s == nil {
		return nil
	}
	cloned := *s
	cloned.Metadata = maps.Clone(s.Metadata)
	cloned.Nodes = slices.Clone(s.Nodes)
	for i, node := range cloned.Nodes {
		if node == nil {
			continue
		}
		n := *node
		n.Metadata = maps.Clone(node.Metadata)
		cloned.Nodes[i] = &n
	}
	cloned.Endpoints = slices.Clone(s.Endpoints)
	for i, endpoint := range cloned.Endpoints {
		if endpoint == nil {
			continue
		}
		e := *endpoint
		e.Metadata = maps.Clone(endpoint.Metadata)
		e.Request = cloneValue(endpoint.Request)
		e.Response = cloneValue(endpoint.Response)
		cloned.Endpoints[i] = &e
	}
	return &cloned
}

// Node 服务节点信息
type Node struct {
	Id       string            `json:"id"`
	Address  string            `json:"address"`
	Port     int               `json:"port"`
	Metadata map[string]string `json:"metadata"`
}

// Endpoint 服务节点信息
type Endpoint struct {
	Name     string            `json:"name"`
	Request  *Value            `json:"request"`
	Response *Value            `json:"response"`
	Metadata map[string]string `json:"metadata"`
}

// Value Value
type Value struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Values []*Value `json:"values"`
}

// cloneValue 递归复制协议描述，避免嵌套字段共享指针。
func cloneValue(value *Value) *Value {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Values = slices.Clone(value.Values)
	for i, child := range cloned.Values {
		cloned.Values[i] = cloneValue(child)
	}
	return &cloned
}
