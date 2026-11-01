# 並行する worktree { #parallel-worktrees }

コーディングエージェントは、複数の `git worktree` で同時に作業することがよくあります。このページでは、
新しい worktree が全件実行なしで役に立つステートを得る方法と、worktree 間で学習内容を共有する方法を
説明します。

## 新しい worktree は記録済みの状態から始まります { #a-new-worktree-starts-warm }

```text
$ git worktree add ../shop-fix-cart -b fix-cart
$ cd ../shop-fix-cart
$ smates run
smates: seeded this worktree from the main worktree (/home/me/src/shop/.smates/state.json.gz)
smates: selected 3 of 412 test unit(s) across 3 suite(s) (99% skipped)
...
```

ステート（`.smates/state.json.gz`）は worktree ごとにあるので、新しい worktree は記録のない状態から
始まり、すべてのテストを選択してしまいます。そうならないよう、ステートのないリンクされた worktree で
最初にステートを必要とするコマンドが、ステートを *シード*（初期化）し、どこから取得したかを 1 行で
表示します。

### シードの取得元 { #where-the-seed-comes-from }

1. メインの worktree にステートがあれば、それを使います。
2. なければ、リモートのステート（`smates state push` で push したもの）を使います。取得は
   [git フック](git-hooks.md) と同じく 1 時間に 1 回までです。ほかの worktree が既に取得したステートの
   ref は、fetch せずにそのまま使います。

### シードが使われる条件 { #when-the-seed-is-kept }

シードには 2 つのものが含まれます。テストごとのエントリ（各テストの依存と前回の結果）と、ツリー全体の
スナップショット（変更の比較基準になるファイルのハッシュ）です。

スナップショットは、この worktree の履歴にあるコミットで記録されたものだけを使います。そうでなければ、
別のブランチの変更をこのブランチの変更のように見せてしまうからです。その場合はテストごとのエントリだけを
残し、スナップショットがないときと同じく、変更ファイルをデフォルトブランチと比べて判定します。

### シードを無効にする { #turn-seeding-off }

- シードは worktree ごとに 1 回だけです。`smates state clear` をすると、再びシードされることはありません。
- `smates.yml` の `state_seed: false` で、全員に対して無効にします。
- `SMATES_NO_STATE_SEED=1` で、その環境に限って無効にします。
- `--state FILE` を明示した場合は、シードも共有もしません。

## worktree 間で 1 つのステートを共有する { #share-one-state-between-worktrees }

シードはステートを 1 回コピーするだけです。*どの* worktree が学習した内容もすべての worktree で共有するには、
次のように設定します。

```yaml
state_shared: true
```

| 内容 | 置き場所 |
|---|---|
| テストごとのエントリ、エージェントの回答、実行記録 | git の共通ディレクトリにある 1 つのファイル: `.git/smates/state.json.gz` |
| ツリー全体のスナップショット | worktree ごと: `.smates/state.snapshot.json.gz` |

スナップショットは worktree ごとに持つので、あるブランチでの実行が、別のブランチの変更をこのブランチの
変更のように見せることはありません。

### 同時に実行した場合 { #concurrent-runs }

複数の worktree の smates が同時に保存しても、互いの結果は失われません。

- 保存時に共有ステートの隣にロックファイルを作り、この実行での変更をディスク上の内容にマージします。
- 2 つの実行が同じテストを再実行した場合は、新しいほうの結果を採用し、結果の履歴は結合します。
- `smates flaky --clear` や `smates state clear` が、それより前に始まった実行によって取り消されることは
  ありません。
- クラッシュしたプロセスが残したロックは、2 分後に引き継がれます。

別の実行がロックを持っていると、保存は最大 1 分待ちます。それでも解放されなければ、その実行は
サマリーを表示したあと終了コード 4（テストが失敗していれば 1）で終わります。もう一度実行してください。

!!! note
    共有モードでの `smates state clear` は、**すべての** worktree の共有ステートを消去します。

## ステートを確認する { #inspect-the-state }

```bash
smates state show                 # ステートの場所、共有かどうか、シードの取得元
smates state show --format json
smates state path                 # ステートファイルのパス（共有モードでは共有ファイル）
```

## 関連項目 { #see-also }

- [git フックと Claude Code](git-hooks.md): フックもリモートのステートを取得します
- [その他の CI](../ci/other-ci.md): `smates state push` と `pull`
- [コマンド](../reference/cli.md#smates-state): `smates state` のすべてのサブコマンド
- [smates.yml リファレンス](../reference/configuration.md): `state`、`state_seed`、`state_shared`
