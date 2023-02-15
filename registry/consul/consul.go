package consul

import ()
import "github.com/huyangv/vmqant/registry"

func NewRegistry(opts ...registry.Option) registry.Registry {
	return registry.NewRegistry(opts...)
}
