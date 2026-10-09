package admin

import "github.com/r3n3r0/nexo/internal/domain"

// CopySourceOf exposes copySourceOf to the external tests.
func CopySourceOf(bucket, key, versionID string) string { return copySourceOf(bucket, key, versionID) }

// PoolStates exposes poolStates to the external tests.
func PoolStates(pools []domain.PoolInfo, nodes []domain.NodeInfo) []domain.PoolInfo {
	return poolStates(pools, nodes)
}
