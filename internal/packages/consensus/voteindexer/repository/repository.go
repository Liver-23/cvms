package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/cosmostation/cvms/internal/common"
	idxmodel "github.com/cosmostation/cvms/internal/common/indexer/model"
	indexerrepo "github.com/cosmostation/cvms/internal/common/indexer/repository"
	dbhelper "github.com/cosmostation/cvms/internal/helper/db"
	"github.com/cosmostation/cvms/internal/packages/consensus/voteindexer/model"
	"github.com/pkg/errors"
	"github.com/uptrace/bun"
)

const IndexName = "voteindexer"

type VoteIndexerRepository struct {
	sqlTimeout time.Duration
	*bun.DB
	indexerrepo.IMetaRepository
}

func NewRepository(indexerDB common.IndexerDB, sqlTimeout time.Duration) VoteIndexerRepository {
	// Instantiate the meta repository
	metarepo := indexerrepo.NewMetaRepository(indexerDB)

	// Return a repository that implements both IMetaRepository and vote-specific logic
	return VoteIndexerRepository{sqlTimeout, indexerDB.DB, metarepo}
}

func (repo *VoteIndexerRepository) InsertValidatorVoteList(
	chainInfoID int64,
	indexPointerHeight int64,
	ValidatorVoteList []model.ValidatorVote,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), repo.sqlTimeout)
	defer cancel()

	// if there are not any miss validators in this block, just update index pointer
	if len(ValidatorVoteList) == 0 {
		_, err := repo.
			NewUpdate().
			Model(&idxmodel.IndexPointer{}).
			Set("pointer = ?", indexPointerHeight).
			Where("chain_info_id = ?", chainInfoID).
			Where("index_name = ?", IndexName).
			Exec(ctx)
		if err != nil {
			return errors.Wrapf(err, "failed to update new index pointer")
		}

		return nil
	}

	// insert miss validators for this block and udpate index pointer in one transaction
	err := repo.RunInTx(
		ctx,
		nil,
		func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.NewInsert().
				Model(&ValidatorVoteList).
				ExcludeColumn("id").
				Exec(ctx)
			if err != nil {
				return errors.Wrapf(err, "failed to insert validator_miss list")
			}

			_, err = tx.
				NewUpdate().
				Model(&idxmodel.IndexPointer{}).
				Set("pointer = ?", indexPointerHeight).
				Where("chain_info_id = ?", chainInfoID).
				Where("index_name = ?", IndexName).
				Exec(ctx)
			if err != nil {
				return errors.Wrapf(err, "failed to update new index pointer")
			}

			return nil
		})

	if err != nil {
		return errors.Wrapf(err, "failed to exec validator miss in a transaction")
	}

	return nil
}

func (repo *VoteIndexerRepository) SelectRecentMissValidatorVoteList(chainID string) ([]model.RecentValidatorVote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), repo.sqlTimeout)
	defer cancel()

	// Make partition table name
	partitionTableName := dbhelper.MakePartitionTableName(IndexName, chainID)

	// Make model
	rvvList := make([]model.RecentValidatorVote, 0)
	query := fmt.Sprintf(`
	SELECT 
		vi.moniker, 
    	MAX(vidx.height) AS max_height,    
    	MIN(vidx.height) AS min_height,
    	COUNT(CASE WHEN status = 1 THEN 1 END) AS missed,
    	COUNT(CASE WHEN status = 2 THEN 1 END) AS commited,
    	COUNT(CASE WHEN status = 3 THEN 1 END) AS proposed
	FROM %s vidx
	JOIN meta.validator_info vi ON vidx.validator_hex_address_id = vi.id
	WHERE height > ((SELECT MAX(height) FROM %s) - 100)
	GROUP BY vi.moniker;
	`, partitionTableName, partitionTableName)
	err := repo.NewRaw(query).Scan(ctx, &rvvList)
	if err != nil {
		return nil, err
	}

	return rvvList, nil
}

func (repo *VoteIndexerRepository) DeleteOldValidatorVoteList(chainID, retentionPeriod string) (
	/* deleted rows */ int64,
	/* unexpected error */ error,
) {
	ctx, cancel := context.WithTimeout(context.Background(), repo.sqlTimeout)
	defer cancel()

	// Parsing retention period
	duration, err := dbhelper.ParseRetentionPeriod(retentionPeriod)
	if err != nil {
		return 0, err
	}

	// Calculate cutoff time duration
	cutoffTime := time.Now().Add(duration)

	// Make partition table name
	partitionTableName := dbhelper.MakePartitionTableName(IndexName, chainID)

	// Query Execution
	res, err := repo.NewDelete().
		Model((*model.ValidatorVote)(nil)).
		ModelTableExpr(partitionTableName).
		Where("timestamp < ?", cutoffTime).
		Exec(ctx)
	if err != nil {
		return 0, err
	}

	rowsAffected, _ := res.RowsAffected()
	return rowsAffected, nil
}
