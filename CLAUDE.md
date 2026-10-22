# dbshift 開發指引

## 專案概述

MySQL → PostgreSQL 資料遷移工具，僅遷移資料（schema 須預先建立）。

## 專案結構（Cobra 官方推薦）

```
main.go                       入口，僅呼叫 cmd.Execute()
cmd/root.go                   Root command、persistent flags、Execute()
cmd/migrate.go                migrate 子命令
cmd/verify.go                 verify 子命令
internal/config/config.go     Config、DBConfig、EnvOr、EnvOrInt
internal/migrator/migrator.go Migrator 核心邏輯、表遷移、型別轉換
internal/migrator/verify.go   資料驗證（行數比對、數值 checksum）
```

## 建置與測試

```bash
go build .                     # 建置
go test ./...                  # 執行所有測試
go vet ./...                   # 靜態分析
```

## 架構要點

- `Migrator` 接收 `*config.Config` 和 `*slog.Logger`（依賴注入）
- 並行遷移使用 semaphore 模式（buffered channel）控制 worker 數量
- 型別轉換集中在 `convertMySQLValue()`：BLOB 保持 `[]byte`、BIT 轉 `bool`/`int64`、其餘 `[]byte` 轉 `string`
- 錯誤處理保留錯誤鏈（`fmt.Errorf("context: %w", err)`）
- 驗證使用行數比對 + 整數欄位 SUM checksum

## 開發慣例

- 測試驅動開發：實作前先寫測試
- table-driven tests（`t.Run` 子測試）
- 不使用全域狀態，所有依賴透過建構函式注入
- `internal/` 下的套件不對外暴露
