package adapter

// BaseAdapter provides common scaffolding for platform adapters.
// Concrete adapters embed this and override platform-specific methods.
type BaseAdapter struct {
	name     string
	capabilities CapSet
}

func (b *BaseAdapter) Name() string {
	return b.name
}

func (b *BaseAdapter) AuthRequired() bool {
	return true
}

func (b *BaseAdapter) Capabilities() CapSet {
	return b.capabilities
}

// SetName sets the adapter name at initialization time.
func (b *BaseAdapter) SetName(n string) {
	b.name = n
}

// SetCapabilities sets the capability bitmask at initialization time.
func (b *BaseAdapter) SetCapabilities(c CapSet) {
	b.capabilities = c
}
