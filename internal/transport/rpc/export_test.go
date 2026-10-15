package rpc

// OpenHandles returns the number of file handles the peers keep open on
// the drives of the storage routes (tests of the handle lifecycle).
func OpenHandles(r StorageRoutes) int { return r.(*storageRoutes).handles.len() }
