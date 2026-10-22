# dbshift

MySQL 到 PostgreSQL 的資料遷移工具。支援多資料庫批次遷移、並行表遷移，以及遷移後的資料驗證。

## 功能

- **多資料庫支援**：一次遷移多個資料庫
- **並行表遷移**：可設定 worker 數量，同時遷移多張表
- **批次 COPY**：使用 PostgreSQL COPY 協定高效寫入
- **型別轉換**：自動處理 MySQL 與 PostgreSQL 之間的型別差異（BLOB、BIT、JSON、DECIMAL、TINYINT→boolean、TIME→time 等）
- **Trigger 管理**：遷移期間自動停用/啟用外鍵與觸發器
- **欄位自動跳過**：來源存在但目標已移除的廢棄欄位會自動跳過並記錄警告，不中斷遷移
- **交易原子性**：每張表的 TRUNCATE、COPY、序列重設在同一筆交易中完成，避免並行 worker 互相干擾
- **Sequence 重設**：遷移後自動修正 PostgreSQL 序列值
- **資料驗證**：比對行數、數值 SUM、文字長度 SUM、日期範圍（MIN/MAX）checksum，確保資料完整性
- **差異診斷**：Checksum 不符時自動 drill-down，顯示不符的欄位與實際差異資料列（可用 `--diagnose-limit` 控制顯示筆數）
- **並行驗證**：驗證階段同樣支援多表並行，MySQL/PG 端查詢同時執行
- **優雅中斷**：支援 Ctrl+C 中斷

## 前提

- MySQL 中的資料庫與表結構須已在 PostgreSQL 中建立（本工具僅遷移資料，不遷移 schema）
- 僅遷移兩端都存在的表（來源有但目標沒有的欄位會自動跳過並發出警告）

## 安裝

```bash
go install github.com/miskawu/dbshift@latest
```

或從原始碼建置：

```bash
git clone https://github.com/MiskaWu/dbshift.git
cd dbshift
go build -o dbshift .
```

## 使用方式

### 基本遷移

```bash
dbshift migrate --databases mydb,mydb_log \
  --mysql-host 10.0.0.1 \
  --pg-host 10.0.0.2
```

### 僅驗證（不遷移）

```bash
dbshift verify --databases mydb
```

### 來源與目標資料庫名稱不同

使用 `src:dst` 格式指定 MySQL 與 PostgreSQL 的資料庫名稱映射：

```bash
dbshift migrate --databases app:app_stage \
  --mysql-host 10.0.0.1 \
  --pg-host pg.example.com --pg-ssl require
```

### 完整範例

```bash
dbshift migrate \
  --databases mydb,mydb_log \
  --mysql-host 10.0.0.1 --mysql-port 3306 --mysql-user root --mysql-pass secret \
  --pg-host 10.0.0.2 --pg-port 5432 --pg-user postgres --pg-pass secret \
  --pg-ssl require \
  --batch 5000 \
  --workers 8 \
  --log-level debug
```

## 子命令

| 子命令 | 說明 |
|--------|------|
| `migrate` | 執行資料遷移（遷移後自動驗證） |
| `verify` | 僅驗證資料一致性，不執行遷移 |

## 全域參數

| 參數 | 環境變數 | 預設值 | 說明 |
|------|----------|--------|------|
| `--databases` | — | （必填） | 要遷移的資料庫，逗號分隔。支援 `src:dst` 格式指定不同的來源/目標名稱 |
| `--mysql-host` | `MYSQL_HOST` | `127.0.0.1` | MySQL 主機 |
| `--mysql-port` | `MYSQL_PORT` | `3306` | MySQL 連接埠 |
| `--mysql-user` | `MYSQL_USER` | `root` | MySQL 使用者 |
| `--mysql-pass` | `MYSQL_PASS` | `""` | MySQL 密碼 |
| `--pg-host` | `PG_HOST` | `127.0.0.1` | PostgreSQL 主機 |
| `--pg-port` | `PG_PORT` | `5432` | PostgreSQL 連接埠 |
| `--pg-user` | `PG_USER` | `postgres` | PostgreSQL 使用者 |
| `--pg-pass` | `PG_PASS` | `""` | PostgreSQL 密碼 |
| `--pg-ssl` | `PG_SSLMODE` | `disable` | PostgreSQL SSL 模式（disable、require、verify-ca、verify-full） |
| `--batch` | — | `1000` | 每批 COPY 的行數 |
| `--workers` | — | `4` | 並行遷移表的 worker 數量 |
| `--diagnose-limit` | — | `10` | 每張表最多顯示幾筆差異列 |
| `--skip-diagnosis` | — | `false` | 跳過 checksum 不符時的逐欄逐行診斷 |
| `--log-level` | — | `info` | 日誌等級（debug、info、warn、error） |

## 遷移流程

1. 連線至 MySQL 與 PostgreSQL
2. 列出 MySQL 中的表，篩選出 PostgreSQL 中也存在的表
3. 對每張表（並行處理）：
   - 停用 PostgreSQL 觸發器
   - 比對來源與目標欄位，跳過目標不存在的廢棄欄位（warn 記錄）
   - 開始交易：清空目標表 → 串流讀取並批次 COPY → 重設序列值 → 提交
   - 啟用觸發器
4. 執行驗證（並行）：比對行數、數值 SUM、文字長度 SUM、日期 MIN/MAX

## 驗證報告

遷移完成後（或使用 `dbshift verify`）會輸出驗證報告：

```
--- mydb ---

TABLE                                       M_COUNT    P_COUNT     CHECKSUM STATUS
-----                                       -------    -------     -------- ------
users                                           100        100        MATCH OK
orders                                           50         48          n/a COUNT_MISMATCH
scores                                           15         15     MISMATCH CHECKSUM_MISMATCH

  Mismatching columns:
  COLUMN                         TYPE             MYSQL                   PG
  score                          numeric          48250                48249

  Sample differing rows (up to 10):
    PK(id=42)  col=score  mysql=95  pg=94

WARNING: mydb has data mismatches
```

Checksum 不符時會自動顯示不符的欄位及差異資料列。可用 `--skip-diagnosis` 關閉，或用 `--diagnose-limit N` 調整顯示筆數。

驗證失敗時程式會以 exit code 1 結束。

## 設計筆記

幾個在正式遷移中踩到、值得記下的決定：

- **驗證要能說明「哪裡不一樣」**：只回報 checksum 不符等於叫人自己去翻資料。不符時自動往下鑽：先逐欄比 SUM 找出哪幾欄不一致，再以主鍵（沒有主鍵就用第一個 unique key）逐列比對，列出實際差異的資料列。
- **checksum 本身也會出錯**：MySQL 的 `CAST(SUM(col) AS SIGNED)` 在總和超過 int64 時會截斷，大表上兩端資料明明一致卻報不符。改成 `CAST(... AS CHAR)` 以十進位字串比對，消除這類假陽性。
- **一張表一筆交易**：TRUNCATE、COPY、序列重設包在同一筆交易裡，並行的 worker 不會看到彼此的半成品；序列先全部查完再逐一重設，避開 pgx 在交易中「上一個結果還沒讀完」的 `conn busy`。
- **目標端的 schema 是準的**：來源有、目標已刪的廢棄欄位直接跳過並記警告，不讓一個歷史欄位擋下整次遷移。

## 授權

[MIT](LICENSE)
