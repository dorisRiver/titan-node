package api

import (
	"context"

	"github.com/Filecoin-Titan/titan/api/types"
)

// Asset is an interface for asset manager
type Asset interface {
	// PullAsset pull the asset with given assetCID from specified sources
	PullAsset(ctx context.Context, assetCID string, sources []*types.CandidateDownloadInfo) error //perm:admin
	// DeleteAsset deletes the asset with given assetCID
	DeleteAsset(ctx context.Context, assetCID string) error //perm:admin
	// GetAssetStats retrieves the statistics of assets
	GetAssetStats(ctx context.Context) (*types.AssetStats, error) //perm:admin
	// GetCachingAssetInfo retrieves the information of assets that are currently being pulled
	GetPullingAssetInfo(ctx context.Context) (*types.InProgressAsset, error) //perm:admin
	// GetAssetProgresses retrieves the progress of assets with specified assetCIDs
	GetAssetProgresses(ctx context.Context, assetCIDs []string) (*types.PullResult, error) //perm:admin
}
