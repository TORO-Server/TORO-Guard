# TORO-Guard — 荒らしを見つけて、証拠の画像を付けて通報し、ロールバックする

TORO サーバーの CoreProtect の記録を見張り、人の手では出せない速さでブロックを壊したり置いたりしている人を見つけたら、

1. 荒らされた場所の画像 (BlueMap の 3D の地図) を撮り、
2. その人のブロックの変更を CoreProtect でロールバックし、
3. 復旧後の画像を撮り、
4. Discord の荒らし通報フォーラムに、投稿を立てて報告する (「ロルバ済」のタグ付き)。

言語は Go。`toro-guard.exe` / `toro-guard` が 1 つで、Chromium (BlueMap の画面を撮る) と一緒にコンテナに入る。

> **状態: 作っただけで、本番には入れていない。** 通報も、ロールバックも、まだ 1 度もしていない (過去の記録に当てはめた試験だけ。下の「確かめたこと」)。

## 流れ

```
CoreProtect (TiDB) ── 3 秒ごとに新しい行 ──▶ 検知 (人の手では出せない速さか)
                                                │ 対象外: #tnt など人でない / TRUSTED_PLAYERS / WorldEdit などのコマンドを使った人
                                                ▼
                                     事件を開く ── 90 秒 動きが止まる (または 10 分たつ) と閉じる
                                                ▼
   証拠の絵 ◀── 真上から見た「壊した場所」の図 (必ず作れる)
   画像 1   ◀── save-all flush → bluemap update → 描き終わるのを待つ → 3D の画面を撮る (被害)
   ロールバック ◀── co rollback u:<名前> t:<秒>s r:#<ワールド>  (終わりはデータベースでも確かめる)
   画像 2   ◀── もう 1 度 BlueMap を更新して撮る (復旧後)
                                                ▼
                         Discord の通報フォーラムに投稿 (荒らしの名前の投稿。同じ人の続きは書き足す)
```

荒らしが続けている間は、ロールバックしない (しても追いつかない)。止まってから、または 10 分ごとに、まとめて処理する。

## 動かし方 (GUARD_MODE)

| モード | することは | しないこと |
| --- | --- | --- |
| `observe` (既定) | 検知して、まとめ (`incident.json`) と真上の図を `DATA_DIR/reports/` に置く | コンソールへの送信、BlueMap の画面、Discord、ロールバック |
| `dry-run` | `observe` に加えて、`save-all flush` と `bluemap update` を送り、BlueMap の画面を撮り、報告文 (`report.md`) を作る。やる予定のロールバックのコマンドを報告文に書く | Discord に投稿しない、ロールバックしない |
| `live` | すべて。ロールバック → 画像 → 投稿 | — |

**まず `observe` で数日、次に `dry-run` で 1 度画像を確かめてから、`live` にする。**

## 検知

プレイヤーごとに、1 秒ごとの数を持って、次のどれかに当たると「荒らしの疑い」。

| 種類 | 既定 | 環境変数 |
| --- | --- | --- |
| 短い間に壊した数 | 5 秒に 100 個以上 | `BURST_WINDOW`, `BREAK_BURST` |
| 短い間に置いた数 | 5 秒に 150 個以上 | `PLACE_BURST` |
| 長い間に壊した数 | 10 分に 1,500 個以上 | `SUSTAINED_WINDOW`, `SUSTAINED_BREAKS` |

(人の手の上限は、1 秒に 4〜8 個ほど。2026-11-04 の fuuty119 は、1 秒に最大 421 個だった。)

次は、検知しても事件にしない (ログに「検知したが対象外」と出る)。

- 名前が `#` で始まる記録 (`#tnt`, `#fire`, `#wither`, `#water` など。人ではない)
- `TRUSTED_PLAYERS` に書いた人、`ops.json` (`OPS_FILE`) にいる op
- `//set`、`/fill`、`/clone`、`/setblock`、`/we`、`/br` などの編集のコマンドを、`WE_GRACE` (既定 30 分) 以内に使った人 (道具で速いのが当たり前)

## 通報

通報先は、フォーラム「荒らし通報」(`1000000000000000001`)。このフォーラムの決まり (トピック: ワールド名・荒らしプレイヤーの名前・証拠画像 5 ブロック以上・画像 1〜2 枚) に合わせる。

- 投稿の名前は、荒らしの名前。本文に、ワールド、名前と UUID、座標、範囲、時間、数、主な破壊、使ったコマンド、対応を書く。
- 画像は 2 枚まで: 被害 → 復旧後 (BlueMap)。撮れなかったときは、真上の図で埋める。
- ロールバックできたら、タグ「ロルバ済」を付ける。
- 同じ人は 24 時間以内なら、新しく立てず、同じ投稿に書き足す。
- このフォーラムでは、ボットに「埋め込みのリンク」の権限が無いので、embed は使わず、普通の文字だけで書く。
- 誤検知のときに戻せるよう、報告に `co restore ...` のコマンドを書く。

### Discord の認証 (DiscordSRV のものを使う)

ボットのトークンは、DiscordSRV の `plugins/DiscordSRV/config.yml` の `BotToken` を、読み取り専用でマウントして読む。トークンを `.env` やリポジトリに写さない。
REST だけを使い、Gateway には接続しない (DiscordSRV が同じトークンで接続しているので、もう 1 本つながない)。

> 2026-11-04 に確かめた: このボット (`TORO chat`) は、通報フォーラムを見られて、投稿・画像の添付ができる。「埋め込みのリンク」だけができない。

## BlueMap の画面

Chromium (`chromedp`) で、`BLUEMAP_URL` の地図を、荒らされた場所の中心 (壊した数の重み付きの中央値) にカメラを向けて開き、タイルが読み終わるまで待って撮る。

- 本番では `BLUEMAP_URL=http://3d-map.torosaba.net` と `BLUEMAP_RESOLVE=toro-edge:80` で、外 (Cloudflare) に出ずに toro-edge から読む。
- BlueMap の `update-cooldown` は 300 秒なので、撮る前に `save-all flush` と `bluemap update <地図> <x> <z> <半径>` を送り、`bluemap` の状態が「待機中・更新済み」になるのを 2 回続けて見るまで待つ (最大 6 分)。
- コンテナの中では GPU が無いので、ソフトウェアの WebGL (SwiftShader) で描く。1 枚あたり 30〜60 秒。
- 撮れなかったときは、画像なし (または真上の図だけ) で続ける。ロールバックも通報も止めない。

## コンソール

toro-main には RCON も shell も無い (`enable-rcon=false`) ので、Docker Engine の API (unix ソケット) で、`docker attach` と同じように標準入力へ 1 行送り、出力を読む (`console.go`)。`/home/marumasa/mccmd.py` と同じ方法。

- ソケットを持つのは root と同じ力。このコンテナは `read_only`、`cap_drop: ALL`、`no-new-privileges`、非 root で動かし、他に渡さない。
- RCON を有効にできたとき (`server.properties` の変更は、`staged/` 経由で次の再起動で入る) は、`Console` の実装を RCON に差し替えれば、ソケットは要らなくなる。

## 設定 (環境変数)

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `GUARD_MODE` | `observe` | `observe` / `dry-run` / `live` |
| `DB_HOST` `DB_PORT` `DB_USER` `DB_PASSWORD` `DB_NAME` `DB_TABLE_PREFIX` | `tidb` `4000` `root` (空) `coreprotect` `coreprotect_` | CoreProtect のデータベース (読むだけ) |
| `BURST_WINDOW` `BREAK_BURST` `PLACE_BURST` `SUSTAINED_WINDOW` `SUSTAINED_BREAKS` | 5s 100 150 10m 1500 | 検知の閾値 |
| `QUIET` `MAX_INCIDENT` | 90s / 10m | 動きが止まって何秒で閉じるか / 長くても何分で閉じるか |
| `WE_GRACE` | 30m | 編集のコマンドを使って、この時間は検知しない |
| `TRUSTED_PLAYERS` `OPS_FILE` | (空) (空) | 検知しない人 (コンマ区切り) / op の一覧 (`ops.json`)。op も検知しない |
| `MAX_EVENT_AGE` | 15m | これより古い行は、見ても報告しない (止まっていたあいだのもの) |
| `DISCORDSRV_CONFIG` | `/discordsrv/config.yml` | DiscordSRV の設定 (BotToken を読む) |
| `REPORT_CHANNEL_ID` `ROLLBACK_TAG_ID` | 通報フォーラム / 「ロルバ済」 | 通報先 |
| `DOCKER_SOCKET` `MC_CONTAINER` | `/var/run/docker.sock` `toro-main` | コンソールの入り口 |
| `KICK_ENABLED` | `false` | `live` で、検知したらすぐキックする |
| `BLUEMAP_URL` `BLUEMAP_RESOLVE` `MAP_IDS` | `http://3d-map.torosaba.net` (空) `main=overworld` | 画面を撮る地図 / 名前の向け先 / ワールド → 地図の名前 |
| `CHROME_PATH` `CHROME_FLAGS` | (探す) | Chromium の場所と、追加のフラグ (コンマ区切り) |
| `SHOT_WIDTH` `SHOT_HEIGHT` `SHOT_SETTLE` | 1280 720 60s | 画像の大きさと、タイルを待つ最大の時間 |
| `WARPS_DIR` | (空) | EssentialsX の `warps/` (報告に「ワープ ○○ の近く」と書く) |
| `DATA_DIR` | `/data` | 状態 (`state.json`) と、報告のフォルダ |

## コマンド

```bash
toro-guard run                      # 見張りを続ける
toro-guard check [-console]         # データベース・Discord のトークンとチャンネルを確かめる (何も投稿しない)
toro-guard scan -days 30            # 過去の記録に、いまの閾値を当てはめて、誤検知を見積もる (何も書かない)
toro-guard replay -user NAME -from 2026-11-04T10:50:00+09:00 -to 2026-11-04T11:00:00+09:00 -out DIR
                                    # 過去の 1 件に当てはめ、画像と報告文を DIR に作る (投稿もロールバックもしない)
```

## 本番に入れる手順 (まだしていない)

1. このリポジトリを `TORO-Server/TORO-Guard` (非公開) に置き、タグ `v*` か main への push で、GHA が `ghcr.io/toro-server/toro-guard` を作る。
2. `compose.example.yaml` のサービスを `/home/toroserver/docker-compose.yaml` に足し、`DOCKER_GID` と `toro-guard-data/` を用意する (GHCR のログインは他の非公開イメージと同じ)。
3. `GUARD_MODE=observe` で起動。`docker exec toro-guard /toro-guard check -console` (つながるか)。
4. 数日見て、検知の記録 (`docker logs toro-guard`) に誤検知が無いか確かめる。
5. `dry-run` にして、`/data/reports/` の画像と `report.md` を確かめる。
6. `live` にする (KICK を使うかも、ここで決める)。

## 確かめたこと (2026-11-04、本番のデータベースを読むだけで)

- **検知**: 過去 30 日の CoreProtect の記録 (約 733 万行) に当てはめて、通報の対象になったのは fuuty119 の 1 件 (10:52:38) だけ。編集のコマンドで対象外になったのが 192 件、人でないものが 20 件。
- **replay**: fuuty119 の記録から、真上の図・BlueMap の画像・報告文ができた。ソフトウェアの WebGL (GPU 無し) でも撮れた (約 60 秒)。
- **daemon**: `dry-run` で、過去の位置から見張りを回し、1 件を検知 → 事件を閉じる → 画像と報告文を作る、までが動いた (投稿なし)。
- **Discord**: DiscordSRV のトークンで、通報フォーラムが見えることを確かめた (何も投稿していない)。
- 単体の試験 (`go test -race`): 検知、コンソールの読み取り、報告文、状態の保存など。

**まだ確かめていないこと**: 実際のロールバックのコマンドの返事 (CoreProtect の文言)、`bluemap update` を送ったあとの待ち、フォーラムへの投稿そのもの (タグ・画像の添付)、コンテナのイメージ (この手元に Docker が無く、ビルドしていない)。`dry-run` と最初の `live` で確かめる。

## 限界

- ロールバックは、その人の変更を、ワールド全体 (`r:#<ワールド>`) で戻す。他の人が、荒らされた場所に置いた物も、その人の変更の上に重なっていると、CoreProtect の決まりで巻き込まれることがある。
- 道具 (WorldEdit など) を使う荒らしは、対象外の決まりで見逃す。コマンドの記録が付くので、人が見られる。
- CoreProtect が記録しないもの (モブの大量発生、パケットの攻撃など) は見られない。
- BlueMap が更新を終えられないとき (長い描画待ち) は、古い地図で撮ることがある (ログに警告が出る)。
