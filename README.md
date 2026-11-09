# dbshift

把 MySQL 的資料搬到 PostgreSQL，搬完自動核對兩邊是否一致。給要換資料庫、需要知道「搬完有沒有少資料」的人：對不上時，報告會指出是哪張表、哪個欄位，能定位時連差異的資料列也列出來。

dbshift 只搬資料，表結構要先在 PostgreSQL 建好。

## 用起來長這樣

一行指令，搬完直接驗證：

```console
$ dbshift migrate --databases shop --log-level warn
time=2026-11-09T08:33:07.700Z level=WARN msg=來源欄位在目標不存在，略過 table=orders column=legacy_flag

--- shop→shop ---

TABLE                                       M_COUNT    P_COUNT     CHECKSUM STATUS
-----                                       -------    -------     -------- ------
customers                                      1000       1000        MATCH OK
orders                                         5000       5000        MATCH OK

shop→shop verification passed
```

`legacy_flag` 是來源有、目標已經沒有的舊欄位，dbshift 跳過它並留一行警告，不中斷遷移。

之後目標端的資料如果被動過，`dbshift verify` 會指出差在哪裡。這裡手動把 `orders` 裡 `id=42` 那一列的金額減 100，再驗證一次：

```console
$ dbshift verify --databases shop

--- shop→shop ---

TABLE                                       M_COUNT    P_COUNT     CHECKSUM STATUS
-----                                       -------    -------     -------- ------
customers                                      1000       1000        MATCH OK
orders                                         5000       5000     MISMATCH CHECKSUM_MISMATCH

  Mismatching columns:
  COLUMN                         TYPE                      MYSQL                   PG
  amount_cents                   numeric                 2648000              2647900

  Sample differing rows (up to 10):
    PK(id=42)  col=amount_cents  mysql=142  pg=42


WARNING: shop→shop has data mismatches
```

發現不一致時程式以 exit code 1 結束，腳本可以據此判斷。

<sub>以上是實際執行的輸出。環境是容器裡的 MySQL 8.4 與 PostgreSQL 18，資料是虛構的（customers 1,000 列、orders 5,000 列），連線設定用環境變數帶入。不一致時，程式後面還會印一行 `Error: verification found mismatches` 和指令用法，這裡省略。</sub>

## 快速開始

需要 Go 1.25 以上。

```bash
go install github.com/miskawu/dbshift@latest
```

或從原始碼建置：

```bash
git clone https://github.com/MiskaWu/dbshift.git
cd dbshift
go build -o dbshift .
```

動手前要知道：

- MySQL 中的資料庫與表結構須已在 PostgreSQL 中建立（本工具僅遷移資料，不遷移 schema）
- 僅遷移兩端都存在的表（來源有但目標沒有的欄位會自動跳過並發出警告）
- 目標端若有遷移範圍以外的表用外鍵參照被遷移的表，遷移會在清空前報錯停止，資料不會變動
- **遷移會先清空所有目標表**；中途失敗時目標可能只有部分資料，重跑一次即可

然後指定要搬的資料庫。密碼走環境變數，寫在 `--mysql-pass`、`--pg-pass` 的話，同一台機器上用 `ps` 就看得到：

```bash
read -rs MYSQL_PASS && export MYSQL_PASS
read -rs PG_PASS && export PG_PASS

dbshift migrate --databases mydb --mysql-host 10.0.0.1 --pg-host 10.0.0.2
```

只想核對、不想動資料，把 `migrate` 換成 `verify`。

## 驗證抓得到什麼

驗證比的是聚合值：列數、整數欄的 SUM、文字欄的字元長度總和、日期欄的 MIN／MAX。聚合值對不上時，才往下逐欄比，再用主鍵（沒有主鍵就用第一個 unique key）逐列找出實際的差異。所以它能很快發現少資料和數值、長度上的落差，但不等於把每一列都比過。

| 目標端的狀況 | 驗證結果 |
|---|---|
| 少了或多了列 | `COUNT_MISMATCH`，列出不符的欄位與差異的列 |
| 整數欄的值變了，總和也跟著變 | `CHECKSUM_MISMATCH`，列出不符的欄位與差異的列 |
| 文字長度變了 | `CHECKSUM_MISMATCH`，列出不符的欄位與差異的列 |
| 日期欄的最早或最晚值變了 | `CHECKSUM_MISMATCH`，列出不符的欄位，但**列不出差異的列**（見下方） |
| 日期欄中間的值變了，最早與最晚不變 | **抓不到**，報告為 `OK` |
| 文字內容變了，但長度相同 | **抓不到**，報告為 `OK` |
| 兩列的整數互換，總和不變 | **抓不到**，報告為 `OK` |
| `DECIMAL`、`FLOAT`、`DOUBLE`、`JSON`、`BLOB`、`ENUM`、`TIME` 等欄位的值變了 | **抓不到**，這些型別不納入比對 |
| 表沒有主鍵也沒有 unique key | 只列出不符的欄位，不列差異的列 |

納入比對的型別：整數是 `int`、`bigint`、`smallint`、`mediumint`、`tinyint`；文字是 `varchar`、`char`、`text`、`tinytext`、`mediumtext`、`longtext`；日期是 `date`、`datetime`、`timestamp`。

目前的限制：不符的欄位裡只要有日期欄（PostgreSQL 的 `date`、`timestamp`、`timestamptz`），逐列比對就會失敗，只留下一行 `逐行差異比對失敗` 的警告，那張表的差異列都列不出來，不符的欄位仍會列出。

逐列比對會把該表的主鍵與不符欄位整張讀進記憶體，表很大時可以用 `--skip-diagnosis` 跳過。

## 功能

- **多資料庫支援**：一次遷移多個資料庫
- **並行表遷移**：可設定 worker 數量，同時遷移多張表
- **批次 COPY**：使用 PostgreSQL COPY 協定高效寫入
- **型別轉換**：自動處理 MySQL 與 PostgreSQL 之間的型別差異（BLOB、BIT、JSON、DECIMAL、TINYINT→boolean、TIME→time 等）
- **一次清空目標表**：worker 開始前用一條 TRUNCATE 清空所有目標表，不用 CASCADE，較晚遷移的父表不會清掉已寫好的子表
- **Trigger 管理**：遷移期間自動停用/啟用外鍵與觸發器
- **欄位自動跳過**：來源存在但目標已移除的廢棄欄位會自動跳過並記錄警告，不中斷遷移
- **交易原子性**：每張表的 COPY、序列重設在同一筆交易中完成
- **Sequence 重設**：遷移後自動修正 PostgreSQL 序列值
- **資料驗證**：比對行數、整數 SUM、文字長度 SUM、日期範圍（MIN/MAX）checksum，範圍見[驗證抓得到什麼](#驗證抓得到什麼)
- **差異診斷**：Checksum 不符時自動 drill-down，顯示不符的欄位與實際差異資料列（可用 `--diagnose-limit` 控制顯示筆數）
- **並行驗證**：驗證階段同樣支援多表並行，MySQL/PG 端查詢同時執行
- **優雅中斷**：支援 Ctrl+C 中斷

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

密碼走環境變數。寫在 `--mysql-pass`、`--pg-pass` 的話，同一台機器上用 `ps` 就看得到。

```bash
read -rs MYSQL_PASS && export MYSQL_PASS
read -rs PG_PASS && export PG_PASS

dbshift migrate \
  --databases mydb,mydb_log \
  --mysql-host 10.0.0.1 --mysql-port 3306 --mysql-user root \
  --pg-host 10.0.0.2 --pg-port 5432 --pg-user postgres \
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
3. 用一條 TRUNCATE 清空所有目標表
4. 對每張表（並行處理）：
   - 停用 PostgreSQL 觸發器
   - 比對來源與目標欄位，跳過目標不存在的廢棄欄位（warn 記錄）
   - 開始交易：串流讀取並批次 COPY → 重設序列值 → 提交
   - 啟用觸發器
5. 執行驗證（並行）：比對行數、整數 SUM、文字長度 SUM、日期 MIN/MAX

## 驗證報告

遷移完成後（或使用 `dbshift verify`）會輸出驗證報告，實際的樣子見[用起來長這樣](#用起來長這樣)。各欄的意思：

- `M_COUNT`、`P_COUNT`：MySQL 與 PostgreSQL 的行數。
- `CHECKSUM`：聚合值比對的結果，`MATCH` 或 `MISMATCH`；表裡沒有可比對的欄位時是 `n/a`。
- `STATUS`：`OK`、`COUNT_MISMATCH`、`CHECKSUM_MISMATCH`；該表查詢失敗時是 `ERROR`。

Checksum 不符時會自動顯示不符的欄位及差異資料列。可用 `--skip-diagnosis` 關閉，或用 `--diagnose-limit N` 調整顯示筆數。

驗證失敗時程式會以 exit code 1 結束。

## 設計筆記

幾個在正式遷移中踩到、值得記下的決定：

- **驗證要能說明「哪裡不一樣」**：只回報 checksum 不符等於叫人自己去翻資料。不符時自動往下鑽：先逐欄比 SUM 找出哪幾欄不一致，再以主鍵（沒有主鍵就用第一個 unique key）逐列比對，列出實際差異的資料列。
- **checksum 本身也會出錯**：MySQL 的 `CAST(SUM(col) AS SIGNED)` 在總和超過 int64 時會截斷，大表上兩端資料明明一致卻報不符。改成 `CAST(... AS CHAR)` 以十進位字串比對，消除這類假陽性。
- **清空和寫入分開**：每個 worker 各自 `TRUNCATE ... CASCADE` 時，父表較晚遷移就會把已寫好的子表清空。改成開工前一次清空所有目標表，不用 CASCADE，外鍵也不會牽連遷移範圍以外的表
- **一張表一筆交易**：COPY、序列重設包在同一筆交易裡，並行的 worker 不會看到彼此的半成品；序列先全部查完再逐一重設，避開 pgx 在交易中「上一個結果還沒讀完」的 `conn busy`。
- **目標端的 schema 是準的**：來源有、目標已刪的廢棄欄位直接跳過並記警告，不讓一個歷史欄位擋下整次遷移。

## 授權

[MIT](LICENSE)
