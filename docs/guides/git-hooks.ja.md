# git フックと Claude Code { #git-hooks-and-claude-code }

smates は、`git push`（またはコミット）の前と、Claude Code がターンを終えるたびに、影響するテストを
自動で実行できます。このページでは、2 種類のフックの導入と削除、そしてその動作を説明します。

## git フック { #a-git-hook }

```text
$ smates hook install
installed the smates pre-push hook in /home/me/src/shop/.git/hooks/pre-push
```

これ以降、`git push` はまず `smates run --no-agent` を実行し、選択されたテストが失敗すると push を
止めます。

| オプション | 動作 |
|---|---|
| `--hook pre-commit` | `pre-push`（既定）の代わりに `pre-commit` フックを入れます |
| `--append` | smates が書いたものではない既存のシェルフックの末尾に smates を追加します |
| `--agent` | オーファンについてフックから [LLM エージェント](agent.md) を呼べるようにします |
| `-- <run のフラグ>` | `smates run` にフラグを渡します。例: `smates hook install -- --explain` |

`--agent` を付けなければ、フックは API を一切呼びません。その場合オーファン（どのテストにも紐づかない
変更）は、影響しうるスイートを丸ごと実行します。[どのテストにも紐づかない変更](orphans.md) を参照して
ください。

### 既存のフック、husky、lefthook { #existing-hooks-husky-and-lefthook }

smates は自分が書いていないフックを上書きしません。

- 通常のフックスクリプトがあれば、処理を止めて追加すべき行を表示します。`--append` を付ければ末尾に
  自分を追記します。スクリプトが `exit` や `exec` で終わっていると、追記したブロックが実行されないため
  警告します。
- husky や lefthook がフックを管理している場合は、何も書き込まずに `.husky/pre-push` や
  `lefthook.yml` 用のスニペットを表示します。

フックは `core.hooksPath` に従い、リポジトリのすべての worktree で共有されます。再度インストールすると、
smates のブロックをその場で更新します。

### フックの動作 { #what-the-hook-does }

push（またはコミット）のたびに、フックは次を行います。

1. **ステートを最新にする。** ローカルにステートがなければ、共有のステートを取得します
   （`smates state pull`）。ローカルのステートが 1 週間より古いか、現在の履歴にないコミットで記録された
   ものなら、リモートのステートをマージします。fetch は 1 時間に 1 回までです。
2. **影響するテストを実行する。** インストール時のフラグで `smates run` を実行します。
3. テストが失敗したら **push を止める。**

!!! warning "フックがテストするのは push するコミットではなく作業ツリーです"
    smates はディスク上のファイルをハッシュします。コミットしていない変更（pre-commit ではステージして
    いない変更）や untracked のファイルもテスト対象に含まれます。そうした変更があるとき、また push する
    ブランチがチェックアウト中のブランチと違うとき、フックは警告します。正確な結果がほしければ、先に
    コミットか stash をしてください。

### スキップする { #skip-it }

- `git push --no-verify`（または `git commit --no-verify`）で 1 回だけスキップします。
- `SMATES_SKIP_HOOK=1` を設定している間はスキップします。
- `smates` が `PATH` にない場合は、警告を出して push を通します。smates を入れていないチームメンバーの
  作業を止めることはありません。

### 状態の確認と削除 { #status-and-removal }

```bash
smates hook status      # フックの場所と、Claude Code のフックが入っているか
smates hook uninstall   # smates が書いた部分だけを削除
```

## Claude Code の Stop フック { #claude-code-stop-hook }

```bash
smates hook install --claude
```

`.claude/settings.json` に Stop フックを追加します。Claude がターンを終えると、フックが影響するテストを
実行します。失敗があると Claude は止まらず、失敗したテストと再実行のコマンドを受け取って修正を続けます。

| オプション | 動作 |
|---|---|
| `--user` | 代わりに `~/.claude/settings.json` に入れます。`smates.yml` のあるすべてのプロジェクトで有効になります |
| `--agent` | オーファンについてフックから LLM エージェントを呼べるようにします（既定は `--no-agent`） |
| `-- <run のフラグ>` | `smates run` に渡すフラグ。例: `-- --suite api` |

### 動作 { #how-it-behaves }

フックは `smates hook claude-stop` を実行し、これが `smates run --output failures --no-agent` に
`--` 以降のフラグを加えて呼び出します。

- **テストが失敗した場合:**
  `{"decision": "block", "reason": "<failing tests and the rerun command>"}` を出力して終了コード 0 で
  終わります。これは Claude に作業を続けさせるための、Claude Code の公式な方法です。Claude は reason を
  読んでテストを直します。
- **二度はブロックしません。** フックの入力に `stop_hook_active` がある（Claude が既に Stop フックに
  よって作業を続けている）ときは、すぐに戻ります。そのためループしません。
- **何も選択されなかった場合:** ステートを書き込まずにすぐ戻ります。`state_shared: true` の場合も
  同じで、そのときは worktree ごとに動きます。
- **smates 自身のエラー**（`smates run` が 2、3、4 で終わるケース）は、Claude をブロックせず、
  終了コード 1 でユーザーに伝えます。ステートを保存できなかっただけなら、失敗したテストは引き続き
  ブロックします。
- `smates` がインストールされていない環境では何もしません。コミットした `.claude/settings.json` が、
  smates のないチームメンバーの環境を壊すことはありません。
- `SMATES_SKIP_HOOK=1` でスキップできます。

### 設定ファイルはそのまま保たれます { #settings-files-stay-intact }

smates は自分のエントリを `settings.json` のテキストに直接差し込みます。

- ほかのすべて（ほかのキーやフック、その順序や書式）は 1 バイトも変わりません。再度インストールしても
  何も変わりません。
- `smates hook uninstall --claude` は、差し込んだテキストと、それによって空になった `Stop` や `hooks`
  だけを削除します。
- 曖昧さなく編集できないファイル（不正な JSON、重複したキー、オブジェクトでない `hooks`、配列でない
  `Stop`）は、報告するだけで手を付けません。

`smates hook status` はプロジェクトとユーザーの両方のフックを表示します。

## 関連項目 { #see-also }

- [コーディングエージェントから使う](coding-agents.md)
- [並行する worktree](worktrees.md): worktree 間でステートを共有する方法
- [コマンド](../reference/cli.md)
