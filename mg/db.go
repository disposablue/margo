package mg

import (
	"sync"
)

var (
	_ KVStore = (KVStores)(nil)
	_ KVStore = (*Store)(nil)
	_ KVStore = (*KVMap)(nil)
)

// KVStore represents a generic key value store.
//
// All operations are safe for concurrent access.
//
// The main implementation in this and related packages is Store.
type KVStore interface {
	// Put stores the value in the store with identifier key
	// NOTE: use Del instead of storing nil
	Put(key, value interface{})

	// Get returns the value stored using identifier key
	// NOTE: if the value doesn't exist, nil is returned
	Get(key interface{}) interface{}

	// Del removes the value identified by key from the store
	Del(key interface{})
}

// KVStores implements a KVStore that duplicates its operations on a list of k/v stores
//
// NOTE: All operations are no-ops for nil KVStores
type KVStores []KVStore

// Put calls .Put on each of k/v stores in the list
func (kvl KVStores) Put(k, v interface{}) {
	for _, kvs := range kvl {
		if kvs == nil {
			continue
		}
		kvs.Put(k, v)
	}
}

// Get returns the first value identified by k found in the list of k/v stores
func (kvl KVStores) Get(k interface{}) interface{} {
	for _, kvs := range kvl {
		if kvs == nil {
			continue
		}
		if v := kvs.Get(k); v != nil {
			return v
		}
	}
	return nil
}

// Del removed the value identified by k from all k/v stores in the list
func (kvl KVStores) Del(k interface{}) {
	for _, kvs := range kvl {
		if kvs == nil {
			continue
		}
		kvs.Del(k)
	}
}

type kvRef struct {
	sync.Mutex
	val interface{}
}

// KVMap implements a KVStore using a map.
// The zero-value is safe for use with all operations.
//
// NOTE: All operations are no-ops on a nil KVMap
type KVMap struct {
	vals map[interface{}]interface{}
	mu   sync.Mutex
}

// Put implements KVStore.Put
func (m *KVMap) Put(k interface{}, v interface{}) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.put(k, v)
}

func (m *KVMap) put(k interface{}, v interface{}) {
	if m.vals == nil {
		m.vals = map[interface{}]interface{}{}
	}
	m.vals[k] = v
}

// Get implements KVStore.Get
func (m *KVMap) Get(k interface{}) interface{} {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	v := m.vals[k]
	m.mu.Unlock()

	return m.unref(v)
}

func (m *KVMap) Ref(k interface{}, new func() interface{}) interface{} {
	if m == nil {
		return new()
	}

	m.mu.Lock()
	ref := m.ref(k)
	m.mu.Unlock()

	ref.Lock()
	defer ref.Unlock()

	if ref.val == nil {
		ref.val = new()
	}
	return ref.val
}

func (m *KVMap) ref(k interface{}) *kvRef {
	v := m.vals[k]
	ref, ok := v.(*kvRef)
	if !ok {
		ref = &kvRef{val: v}
		m.put(k, ref)
	}
	return ref
}

func (m *KVMap) unref(v interface{}) interface{} {
	ref, boxed := v.(*kvRef)
	if !boxed {
		return v
	}

	ref.Lock()
	defer ref.Unlock()

	return ref.val
}

// Del implements KVStore.Del
func (m *KVMap) Del(k interface{}) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.vals, k)
}

// Clear removes all values from the store
func (m *KVMap) Clear() {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.vals = nil
}

// Values returns a copy of all values stored
func (m *KVMap) Values() map[interface{}]interface{} {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	vals := make(map[interface{}]interface{}, len(m.vals))
	for k, v := range m.vals {
		if v := m.unref(v); v != nil {
			vals[k] = v
		}
	}
	return vals
}
