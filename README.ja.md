# gmail-filter-sync

Gmail のフィルターを Jsonnet ファイルで管理し、`gws` コマンド経由で同期するツールです。実行時には `gws` が必要です。

## インストール

[GitHub Releases](https://github.com/ngyuki/gmail-filter-sync/releases) から OS・アーキテクチャに合ったバイナリをダウンロードできます。
実行権限を付け、`gmail-filter-sync` という名前で PATH の通ったディレクトリに配置してください。

Go を使ってインストールすることもできます。

```sh
go install github.com/ngyuki/gmail-filter-sync@latest
```

## 準備

[`gws`](https://github.com/googleworkspace/cli/releases) をダウンロードしてインストールしてください。
初回は `gws auth setup` を実行するか、[公式の手順](https://github.com/googleworkspace/cli#authentication)に従って OAuth 認証情報を手動で設定します。
その後、`gws auth login` で対象アカウントを認証してください。
認証時の TUI で次の 2 つのスコープを有効にしてください。

- `gmail.settings.basic`：フィルターの取得・作成・削除
- `gmail.labels`：ラベル一覧の取得

## 使い方

現在の設定を取得し、ファイルを編集して差分を確認し、変更点を Gmail に適用します。

```sh
gmail-filter-sync import # Gmail の設定をファイルに取り込む
gmail-filter-sync edit   # 設定ファイルを編集する
gmail-filter-sync diff   # 変更点を確認する
gmail-filter-sync apply  # 変更点を Gmail に適用する
```

`import` は既存ファイルを上書きしません。更新する場合は `--force` を指定します。取得した設定は JSON 互換の Jsonnet として保存します。

フィルタ設定ファイルは `--file FILE`、環境変数 `GMAIL_FILTER_SYNC_FILE`、デフォルトの順で決まります。
`--file` を省略し、`GMAIL_FILTER_SYNC_FILE` が未設定または空の場合は `$XDG_CONFIG_HOME/gmail-filter-sync/filter.jsonnet` を使います。
`XDG_CONFIG_HOME` が未設定・空・相対パスの場合は `~/.config/gmail-filter-sync/filter.jsonnet` を使います。

```sh
export GMAIL_FILTER_SYNC_FILE=./filter.jsonnet
gmail-filter-sync diff
```

`edit` は `$EDITOR` でファイルを開きます。`EDITOR="code --wait"` のように引数も指定できます。`EDITOR` が未設定の場合はエラーです。
ファイルがない場合は親ディレクトリを作成し、ファイルの作成はエディターに任せます。

`diff` は Gmail のフィルター設定との差分を unified diff 形式で表示します。`apply` は差分を適用します。

環境変数 `GMAIL_FILTER_SYNC_DIFF_FILTER` にシェルコマンドを指定して差分表示を加工できます。標準入力に unified diff が渡されます。
たとえば `GMAIL_FILTER_SYNC_DIFF_FILTER='colordiff | diff-highlight'` とすると、色付けして表示できます。`diff` と `apply` の両方に適用されます。

```sh
export GMAIL_FILTER_SYNC_DIFF_FILTER='colordiff | diff-highlight'
gmail-filter-sync diff
```

対象アカウントは `--user USER`、環境変数 `GMAIL_FILTER_SYNC_USER` の優先順で決まります。
`--user` を省略し、`GMAIL_FILTER_SYNC_USER` が未設定または空の場合は、`gws` が認証しているユーザー（`me`）を使います。

```sh
export GMAIL_FILTER_SYNC_USER=user@example.com
gmail-filter-sync diff
```

## ファイル形式

[example/filter.jsonnet](example/filter.jsonnet) に例があります。
`filters` 配列の各要素に [Gmail API の criteria と action](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.filters) を記述します。

```jsonnet
local sender = 'newsletter@example.com';
{
  filters: [{
    criteria: { from: sender },
    action: {
      removeLabels: ['INBOX'],
      addLabels: ['STARRED'],
    },
  }],
}
```

`criteria` は `from`、`to`、`subject`、`query`、`negatedQuery`、`hasAttachment`、`excludeChats`、`size`、`sizeComparison` に対応します。
サイズ指定では `size`（バイト数）と `sizeComparison`（`larger` または `smaller`）を組み合わせます。

`query` の文字列はそのまま Gmail に渡します。複数の検索語句をまとめる場合は、Jsonnet の配列と `std.join` を使って検索文字列を組み立てられます。

```jsonnet
local recipients = ['announce@example.com', 'no-reply@example.com'];
{
  filters: [{
    criteria: {
      query: 'from:{%s}' % std.join(' ', recipients),
    },
    action: { addLabels: ['STARRED'] },
  }],
}
```

これは `from:{announce@example.com no-reply@example.com}` として Gmail に渡します。

`action` は `addLabels`、`removeLabels`、`forward` に対応します。
ラベルには Gmail に表示される名前を指定します。同期時に Gmail のラベル一覧から ID に変換します。名前は大文字・小文字を含めて一致させてください。存在しない名前はエラーになります。

カスタムラベルの追加はフィルターごとに 1 個までです。転送先は Gmail 側で事前に確認済みのアドレスを指定してください。
詳しくは [公式のフィルター管理ガイド](https://developers.google.com/workspace/gmail/api/guides/filter_settings)を参照してください。

## 同期の動作

ファイルをアカウント全体のフィルター設定として扱います。
ファイルにない既存フィルターは削除対象です。変更は新規作成と旧設定の削除で反映し、内容が一致するものはそのまま残します。
フィルターとラベルの並び順、空文字列、空配列、既定値の `false` は差分になりません。検索文字列そのものの同義判定は行いません。

全削除には `{ filters: [] }` と `apply` サブコマンドに `--allow-empty` が必要です。

追加をすべて終えてから削除を開始します。作成中に失敗した場合は既存設定の削除に進みません。
API 操作はトランザクションではないため、途中で失敗した場合は一部の変更が残る可能性があります。
再度 `diff` で状態を確認してから適用してください。

作成を先行するため、作成時点で上限の 1,000 件を超える場合は適用を拒否します。一時的に新旧フィルターが共存します。
適用後は設定を再取得し、ファイルの内容と一致することを確認します。

## テスト

```sh
make check
make test
```

テストでは `gws` の応答を模擬し、差分判定、適用順序、失敗時の中断などを検証します。実際の Gmail での適用は含みません。
