package asset

import (
	"context"
	"fmt"
	"time"

	"github.com/Filecoin-Titan/titan/api"
	"github.com/Filecoin-Titan/titan/api/types"
	"github.com/Filecoin-Titan/titan/node/asset/storage"
	"github.com/Filecoin-Titan/titan/node/ipld"
	"github.com/gbrlsnchs/jwt/v3"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-libipfs/blocks"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"
)

var log = logging.Logger("asset")

type Asset struct {
	scheduler       api.Scheduler
	mgr             *Manager
	TotalBlockCount int
	apiSecret       *jwt.HMACSHA
	AWS
}

// NewAsset creates a new Asset instance
func NewAsset(storageMgr *storage.Manager, scheduler api.Scheduler, assetMgr *Manager, apiSecret *jwt.HMACSHA) *Asset {
	return &Asset{
		scheduler: scheduler,
		mgr:       assetMgr,
		apiSecret: apiSecret,
		AWS:       NewAWS(scheduler, storageMgr),
	}
}

// PullAsset adds the asset to the waitList for pulling
func (a *Asset) PullAsset(ctx context.Context, rootCID string, infos []*types.CandidateDownloadInfo) error {
	if types.RunningNodeType == types.NodeEdge && len(infos) == 0 {
		return fmt.Errorf("candidate download infos can not empty")
	}

	root, err := cid.Decode(rootCID)
	if err != nil {
		return err
	}

	has, err := a.mgr.AssetExists(root)
	if err != nil {
		return err
	}

	if has {
		log.Debugf("Asset %s already exist", root.String())
		return nil
	}

	log.Debugf("Cache asset %s", rootCID)

	a.mgr.addToWaitList(root, infos)
	return nil
}

// DeleteAsset deletes the asset with the given CID
func (a *Asset) DeleteAsset(ctx context.Context, assetCID string) error {
	c, err := cid.Decode(assetCID)
	if err != nil {
		log.Errorf("Decode asset cid %s error: %s", assetCID, err.Error())
		return err
	}

	log.Debugf("DeleteAsset %s", assetCID)

	go func() {
		if err := a.mgr.DeleteAsset(c); err != nil {
			log.Errorf("delete asset failed %s", err.Error())
			return
		}

		_, diskUsage := a.mgr.GetDiskUsageStat()
		ret := types.RemoveAssetResult{BlocksCount: a.TotalBlockCount, DiskUsage: diskUsage}

		if err := a.scheduler.NodeRemoveAssetResult(context.Background(), ret); err != nil {
			log.Errorf("remove asset result failed %s", err.Error())
		}
	}()

	return nil
}

// GetAssetStats returns statistics about the assets stored on this node
func (a *Asset) GetAssetStats(ctx context.Context) (*types.AssetStats, error) {
	assetCount, err := a.mgr.AssetCount()
	if err != nil {
		return nil, err
	}

	assetStats := &types.AssetStats{}
	assetStats.TotalBlockCount = a.TotalBlockCount
	assetStats.TotalAssetCount = assetCount
	assetStats.WaitCacheAssetCount = a.mgr.waitListLen()
	_, assetStats.DiskUsage = a.mgr.GetDiskUsageStat()

	puller := a.mgr.puller()
	if puller != nil {
		assetStats.InProgressAssetCID = puller.root.String()
	}

	log.Debugf("asset stats: %#v", *assetStats)

	return assetStats, nil
}

// GetPullingAssetInfo returns information about the asset currently being pulled
func (a *Asset) GetPullingAssetInfo(ctx context.Context) (*types.InProgressAsset, error) {
	puller := a.mgr.puller()
	if puller == nil {
		return nil, fmt.Errorf("no asset caching")
	}

	ret := &types.InProgressAsset{}
	ret.CID = puller.root.Hash().String()
	ret.TotalSize = int64(puller.totalSize)
	ret.DoneSize = int64(puller.doneSize)

	return ret, nil
}

// GetBlocksOfAsset returns a random subset of blocks for the given asset.
func (a *Asset) GetBlocksOfAsset(assetCID string, randomSeed int64, randomCount int) ([]string, error) {
	root, err := cid.Decode(assetCID)
	if err != nil {
		return nil, err
	}

	return a.mgr.GetBlocksOfAsset(root, randomSeed, randomCount)
}

// BlockCountOfAsset returns the block count for the given asset.
func (a *Asset) BlockCountOfAsset(assetCID string) (int, error) {
	c, err := cid.Decode(assetCID)
	if err != nil {
		return 0, err
	}

	count, err := a.mgr.GetBlockCount(context.Background(), c)
	if err != nil {
		return 0, err
	}

	return count, nil
}

// CreateAsset notify candidate that user upload asset, return auth token of candidate
func (a *Asset) CreateAsset(ctx context.Context, tokenPayload *types.AuthUserUploadDownloadAsset) (string, error) {
	c, err := cid.Decode(tokenPayload.AssetCID)
	if err != nil {
		return "", err
	}

	v, ok := a.mgr.uploadingAssets.Load(c.Hash().String())
	if ok {
		asset := v.(*types.UploadingAsset)
		if asset.TokenExpiration.After(time.Now()) {
			return "", fmt.Errorf("asset %s already uploading", tokenPayload.AssetCID)
		}
	}

	asset := &types.UploadingAsset{UserID: tokenPayload.UserID, TokenExpiration: tokenPayload.Expiration, Progress: &types.UploadProgress{}}
	a.mgr.uploadingAssets.Store(c.Hash().String(), asset)

	tk, err := jwt.Sign(&tokenPayload, a.apiSecret)
	if err != nil {
		return "", err
	}

	return string(tk), nil
}

// GetAssetProgresses returns the progress of the given list of assets.
func (a *Asset) GetAssetProgresses(ctx context.Context, assetCIDs []string) (*types.PullResult, error) {
	progresses := make([]*types.AssetPullProgress, 0, len(assetCIDs))
	for _, assetCID := range assetCIDs {
		root, err := cid.Decode(assetCID)
		if err != nil {
			log.Errorf("decode cid %s", err.Error())
			return nil, err
		}

		progress, err := a.progress(root)
		if err != nil {
			log.Errorf("get asset progress %s", err.Error())
			return nil, err
		}
		progresses = append(progresses, progress)
	}

	result := &types.PullResult{
		Progresses:       progresses,
		TotalBlocksCount: a.TotalBlockCount,
	}

	if count, err := a.mgr.AssetCount(); err == nil {
		result.AssetCount = count
	}
	_, result.DiskUsage = a.mgr.GetDiskUsageStat()

	return result, nil
}

// progressForAssetPulledSucceeded returns asset pull progress for the succeeded asset.
func (a *Asset) progressForAssetPulledSucceeded(root cid.Cid) (*types.AssetPullProgress, error) {
	progress := &types.AssetPullProgress{
		CID:    root.String(),
		Status: types.ReplicaStatusSucceeded,
	}

	count, err := a.mgr.GetBlockCount(context.Background(), root)
	if err != nil {
		return nil, xerrors.Errorf("get block count %w", err)
	}

	progress.BlocksCount = count
	progress.DoneBlocksCount = count

	blk, err := a.mgr.GetBlock(context.Background(), root, root)
	if err != nil {
		return nil, xerrors.Errorf("get block %w", err)
	}

	blk = blocks.NewBlock(blk.RawData())
	linksSize := uint64(len(blk.RawData()))

	// TODO check blk data type
	node, err := ipld.DecodeNode(context.Background(), blk)
	if err == nil {
		for _, link := range node.Links() {
			linksSize += link.Size
		}
	} else {
		log.Warnf("decode node %s", err.Error())
	}

	progress.Size = int64(linksSize)
	progress.DoneSize = int64(linksSize)

	return progress, nil
}

func (a *Asset) progress(root cid.Cid) (*types.AssetPullProgress, error) {
	status, err := a.mgr.assetStatus(root)
	if err != nil {
		return nil, xerrors.Errorf("asset %s cache status %w", root.Hash(), err)
	}

	switch status {
	case types.ReplicaStatusWaiting:
		return &types.AssetPullProgress{CID: root.String(), Status: types.ReplicaStatusWaiting}, nil
	case types.ReplicaStatusPulling:
		return a.mgr.progressForPulling(root)
	case types.ReplicaStatusFailed:
		return a.mgr.progressForAssetPulledFailed(root)
	case types.ReplicaStatusSucceeded:
		return a.progressForAssetPulledSucceeded(root)
	}
	return nil, xerrors.Errorf("unknown asset %s status %d", root.String(), status)
}

func (a *Asset) GetAssetView(ctx context.Context) (*types.AssetView, error) {
	topHash, err := a.mgr.GetTopHash(ctx)
	if err != nil {
		return nil, err
	}

	bucketHashes, err := a.mgr.GetBucketHashes(ctx)
	if err != nil {
		return nil, err
	}

	return &types.AssetView{TopHash: topHash, BucketHashes: bucketHashes}, nil
}

func (a *Asset) GetAssetsInBucket(ctx context.Context, bucketID int) ([]string, error) {
	cids, err := a.mgr.Storage.GetAssetsInBucket(ctx, uint32(bucketID))
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0)
	for _, cid := range cids {
		hashes = append(hashes, cid.Hash().String())
	}
	return hashes, nil
}
