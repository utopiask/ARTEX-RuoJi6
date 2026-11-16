package db

// 共享资产筛选构造器 + 导出流式读取（实施方案 §10.1）：列表/数量/导出使用
// 同一个 WHERE 组合（dsl+type+company+task 全部 AND 组合），最小修复既有
// 「DSL 与 company 组合时 company 被忽略」的不一致，不重写 DSL 引擎。

import (
	"context"
	"database/sql"
	"fmt"
)

// AssetFilter is the composable asset list/export filter.
type AssetFilter struct {
	DSL       string
	Type      string
	CompanyID int64
	TaskID    int64
}

// BuildAssetFilterWhere compiles the filter into one WHERE clause + positional
// args（不含 ORDER/LIMIT）。DSL 非空时先 ValidateDSL 语义编译。
func BuildAssetFilterWhere(f AssetFilter) (string, []any, error) {
	where := "true"
	args := []any{}
	if f.DSL != "" {
		node, err := ParseDSL(f.DSL)
		if err != nil {
			return "", nil, err
		}
		dslWhere, dslArgs, err := buildDSLWhere(node)
		if err != nil {
			return "", nil, err
		}
		args = append(args, dslArgs...)
		where = "(" + dslWhere + ")"
	}
	if f.Type != "" {
		args = append(args, f.Type)
		where += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if f.CompanyID > 0 {
		args = append(args, f.CompanyID)
		where += fmt.Sprintf(" AND company_id = $%d", len(args))
	}
	if f.TaskID > 0 {
		args = append(args, f.TaskID)
		where += fmt.Sprintf(" AND $%d = ANY(task_ids)", len(args))
	}
	return where, args, nil
}

// CountAssetsFiltered returns the total row count for a filter.
func (s *AssetStore) CountAssetsFiltered(f AssetFilter) (int, error) {
	where, args, err := BuildAssetFilterWhere(f)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRow("SELECT count(*) FROM assets WHERE "+where, args...).Scan(&n)
	return n, err
}

// QueryAssetsFiltered returns one page in the canonical list order.
func (s *AssetStore) QueryAssetsFiltered(f AssetFilter, limit, offset int) ([]*Asset, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where, args, err := BuildAssetFilterWhere(f)
	if err != nil {
		return nil, err
	}
	args = append(args, limit, offset)
	q := assetSelectCols + " WHERE " + where +
		fmt.Sprintf(" ORDER BY last_seen DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if f.TaskID > 0 {
		if err := s.hydrateTaskAssetSources(f.TaskID, assets); err != nil {
			return nil, err
		}
	}
	return assets, nil
}

// StreamAssetsForExport reads every matching asset in one REPEATABLE READ
// snapshot（一致性快照，§10.1），按 (type,id) 稳定顺序分批回调。不靠超大
// size 突破列表 200 上限；批次大小固定 500。
func (s *AssetStore) StreamAssetsForExport(ctx context.Context, f AssetFilter, ids []int64, fn func(batch []*Asset) error) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	where := "true"
	var args []any
	if len(ids) > 0 {
		args = append(args, toArrayInt64(ids))
		where = "id = ANY($1::bigint[])"
	} else {
		built, bargs, err := BuildAssetFilterWhere(f)
		if err != nil {
			return 0, err
		}
		where = built
		args = bargs
	}
	total := 0
	lastType, lastID := "", int64(0)
	const batch = 500
	for {
		// 每页只把自己的参数放进参数表：筛选参数 + （非首页）游标 + LIMIT，
		// 不留未引用参数（PG 对未出现于语句的参数报 42P18）。
		pageArgs := append([]any{}, args...)
		wherePage := where
		if total > 0 {
			pageArgs = append(pageArgs, lastType, lastID)
			wherePage += fmt.Sprintf(" AND (type, id) > ($%d::text, $%d::bigint)", len(pageArgs)-1, len(pageArgs))
		}
		pageArgs = append(pageArgs, batch)
		q := assetSelectCols + " WHERE " + wherePage + " ORDER BY type, id" + fmt.Sprintf(" LIMIT $%d", len(pageArgs))
		rowsQ, err := tx.Query(q, pageArgs...)
		if err != nil {
			return 0, err
		}
		assets, err := scanAndClose(rowsQ)
		if err != nil {
			return 0, err
		}
		if len(assets) == 0 {
			return total, nil
		}
		total += len(assets)
		if err := fn(assets); err != nil {
			return 0, err
		}
		last := assets[len(assets)-1]
		lastType, lastID = last.Type, last.ID
		if len(assets) < batch {
			return total, nil
		}
	}
}
