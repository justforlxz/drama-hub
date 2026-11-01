package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SourceState 是 source_state 表的一行：一个数据源的调度进展（docs/SCHEDULER-MODEL.md）。
//
// 它与 settings 分表：settings 是运行配置（人写），本表是调度进展（框架写）。
// 五个字段全部是不透明 TEXT（UTC RFC3339 或插件给的游标文本），store 不解析、
// 不格式化、不校验；空串表示「还没发生过」。
//
// 库里不存 next_due_at：它由 LastRunAt + 当前 interval 现算，存下来就是第二份真值。
type SourceState struct {
	Name      string // 等于 Plugin.Name()，主键
	Cursor    string // 插件枚举的不透明断点；全库枚举跨重启续跑的唯一依据
	LastRunAt string // 最近一次开始跑一轮的时间；算 next_due 的唯一真值来源
	LastOkAt  string // 最近一次成功完成一轮的时间
	LastError string // 最近一次失败的文本与时间；成功一轮后清空
}

// sourceStateColumns 是 source_state 的显式列清单，顺序必须与 scanState 一致。
const sourceStateColumns = `SELECT name, cursor, last_run_at, last_ok_at, last_error FROM source_state`

// scanState 按 sourceStateColumns 的顺序读出一行。
func scanState(scan func(dest ...any) error) (SourceState, error) {
	var st SourceState
	if err := scan(&st.Name, &st.Cursor, &st.LastRunAt, &st.LastOkAt, &st.LastError); err != nil {
		return SourceState{}, err
	}
	return st, nil
}

// LoadState 读取某个数据源的调度状态。
//
// 无行返回 ErrNotFound —— 「这个源还没跑过任何一轮」是调用方要区分的情形
// （S4 启动时据此插入一行空状态），所以不返回零值冒充成功。
func (s *Store) LoadState(ctx context.Context, name string) (SourceState, error) {
	row := s.db.QueryRowContext(ctx, sourceStateColumns+` WHERE name = $1`, name)
	st, err := scanState(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceState{}, ErrNotFound
	}
	if err != nil {
		return SourceState{}, fmt.Errorf("读取数据源状态 %s: %w", name, err)
	}
	return st, nil
}

// SaveState 以 name 为身份写入或更新一个数据源的调度状态。
//
// 五个字段全量覆盖：上游契约里「cursor 只在成功推进枚举后写回」「成功一轮后
// 清空 last_error」都是调用方的语义，store 不做部分更新，避免出现「只改了
// last_error 却把 cursor 冲成零值」这类静默丢进度的写入。
func (s *Store) SaveState(ctx context.Context, st SourceState) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO source_state(name, cursor, last_run_at, last_ok_at, last_error)
		 VALUES($1, $2, $3, $4, $5)
		 ON CONFLICT(name) DO UPDATE
		 SET cursor = excluded.cursor,
		     last_run_at = excluded.last_run_at,
		     last_ok_at = excluded.last_ok_at,
		     last_error = excluded.last_error`,
		st.Name, st.Cursor, st.LastRunAt, st.LastOkAt, st.LastError,
	); err != nil {
		return wrapWriteErr("写入数据源状态 "+st.Name, err)
	}
	return nil
}
