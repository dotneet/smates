# LLM エージェント { #the-llm-agent }

smates は、決定的な解析ではこなせない 2 つの仕事のために、小さな LLM（*エージェント*）を呼び出せます。
このページでは、エージェントを呼ぶタイミング、コスト、そして安全性を保つための制約を説明します。

## 有効にする { #turn-it-on }

API キーを 1 つ設定するだけです。

```bash
export ANTHROPIC_API_KEY=sk-ant-...
smates run
```

GitHub Actions ではアクションにキーを渡します
（`anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}`）。[GitHub Actions](../ci/github-actions.md)
を参照してください。キーがなくても smates はまったく同じように動き、影響しうるスイートを丸ごと
実行する方法にフォールバックします。

## 呼び出されるとき { #when-smates-calls-it }

- **オーファン。** どのテストも依存していない変更ファイルです（[どのテストにも紐づかない変更](orphans.md)
  を参照）。エージェントは、それがどのテストに影響しうるか（あるいはどれにも影響しないか）を判断します。
- **失敗の説明。** 失敗した実行のあと、いくつかの失敗について推定原因を 1 行で示します
  （[失敗ヒント](failure-hints.md#agent-explanations) を参照）。

ほとんどの pull request はどちらにも当てはまらないため、LLM を一度も呼びません。

## プロバイダー { #providers }

smates には 3 つのプロバイダー向けのクライアントが組み込まれています（SDK は使いません）。
`provider: auto`（既定）では、次の表の上から順に、キーが設定されている最初のプロバイダーを使います。

| プロバイダー | API キー（ベース URL の上書き） | API | 既定のモデル |
|---|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` または `ANTHROPIC_AUTH_TOKEN`（`ANTHROPIC_BASE_URL`） | Messages | `claude-haiku-5-5` |
| `openai` | `OPENAI_API_KEY`（`OPENAI_BASE_URL`） | Responses | `gpt-6-luna` |
| `openrouter` | `OPENROUTER_API_KEY`（`OPENROUTER_BASE_URL`） | Chat Completions | `anthropic/claude-haiku-5.5` |

```yaml
agent:
  provider: openai          # auto | anthropic | openai | openrouter
  model: gpt-6-luna         # プロバイダーの既定モデルを上書き
  max_turns: 12
  timeout: 120s
```

`agent.enabled: never` にすると、失敗の説明も含めてエージェントを常に無効にします。1 回の実行だけ
無効にするなら `smates run --no-agent` を使います。

## コストと所要時間 { #cost-and-latency }

既定では推論の effort を low にし、プロンプトキャッシュを使います。OpenAI と OpenRouter では、effort を
受け付けないモデルにはそれを外して送り直します。Anthropic では effort を拒否されると呼び出しが失敗し、
ほかの失敗と同じくフォールバックします（[制約とフォールバック](#bounds-and-fallbacks) を参照）。
1 回の呼び出しは通常 $0.01 を大きく下回り、数秒で終わります。

`smates stats` はエージェントの呼び出し回数とトークン数を集計します（[シャドーモード・flaky・統計](trust.md#stats)
を参照）。組み込みの Anthropic・OpenAI・OpenRouter のクライアントはコストを報告しないので、`stats` に
コストが出るのは、出力に `total_cost_usd` を含む [`agent.command`](#an-external-agent-agentcommand) を
使う場合だけです。

## エージェントにできること { #what-the-agent-can-do }

オーファンについて、エージェントは **読み取り専用のツール** でリポジトリを調べます。`list_files`、
`grep`、`read_file`、`git_diff`、`tests_for_file`、`test_deps`、`search_tests` で、いずれもリポジトリ内に
限られます。`.env` のように git が無視するファイルは読めません。

回答は次のいずれかです。

- 追加する特定のテスト
- 影響しうるスイートの全件実行
- 影響なし

エージェントは **テストを削除しません**。回答にできるのは、決定的な選択の結果にテストを追加する
ことだけです。誤った回答や操作された回答でも、テストを追加し損ねるだけで、テストを隠すことは
できません。

## 制約とフォールバック { #bounds-and-fallbacks }

- 各呼び出しには `max_turns`（既定 12）、実時間の `timeout`（既定 `120s`）、入力トークンの上限が
  あります。
- 回答は実際のテスト一覧と照合します。存在しないテスト名は除き、組み込みのバックエンドは一度だけ
  修正を求めます。テストを挙げたのに 1 つも存在しない回答は、空の選択ではなく失敗として扱います。
- どんな失敗（キーなし、ネットワークエラー、タイムアウト、不正な回答）でも、影響しうるスイートを
  丸ごと実行する方法にフォールバックし、ログにその理由を出します。

## キャッシュ { #caching }

回答はステートにキャッシュされます。キーはオーファン（パス、状態、内容）、プロバイダー・モデル・effort
（`agent.command` を使う場合はそのコマンド）、`agent.instructions`、テスト一覧、smates のプロンプトの
バージョンです。テスト一覧が同じなら、同じ変更について二度問い合わせることはありません。そのときのログは
`agent: reusing cached decision for N orphan file(s)` です。プロンプトを変えた smates のリリースでは
問い合わせ直します。

失敗の説明も同様に、テスト、変更された入力、失敗出力をキーにキャッシュされます。

## シャード実行 { #sharded-runs }

シャード実行（`--shard I/N`）ではエージェントに問い合わせません。各シャードが別々に問い合わせると、
あるシャードの回答だけが選んだテストが別のシャードの担当で、どこでも実行されない恐れがあるためです。

シャードは、復元したステートにキャッシュされた判断（同じ変更に対するシャードなしの `smates select`
や `smates run` の結果）を再利用します。それがなければオーファンポリシーにフォールバックし、その旨を
表示します。

## プロジェクト固有の指示 { #project-specific-instructions }

`agent.instructions` はシステムプロンプトの末尾に追加されます。コードからは分からない事実を伝えるのに
使います。

```yaml
agent:
  instructions: "Files under deploy/ configure production only. db/seed.sql is loaded by tests/db."
```

## 外部のエージェント: `agent.command` { #an-external-agent-agentcommand }

組み込みのクライアントの代わりに、任意のエージェント CLI を使えます。

```yaml
agent:
  command: "fx ask --json --no-save"
```

- smates はリポジトリのルートで `sh -c` によりコマンドを実行し、プロンプトを標準入力に書き込み、
  回答を標準出力から読みます。プロンプトの末尾には回答の JSON スキーマ（`decision` は `none`・
  `tests`・`run_all` のいずれか、ほかに `tests`・`no_impact`・`summary`）が付きます。`tests` の
  エントリに `"test": "*"` を指定すると、そのスイート全体を追加します。
- 回答は、標準出力のうちトップレベルに `decision` キーを持つ最後の JSON オブジェクトです（そのままでも、
  markdown のコードブロックの中でも構いません）。標準出力全体が `decision` を持たない 1 つの JSON
  オブジェクトなら、それはエンベロープとして扱います。smates は回答をその `final_output` か `result`
  フィールド（文字列でもオブジェクトでも可）から取り出し、`smates stats` のために `usage`
  （`input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`）、
  `num_turns`、`total_cost_usd` を読みます。
- タイムアウト、検証、キャッシュ、フォールバックは組み込みのクライアントと同じです。
- コマンドを使うのはオーファンだけです（[失敗の説明](#failure-explanations) は無効になります）。

プロセスの扱いは、smates に端末があるかどうかで変わります。

| smates の実行環境 | プロセスグループ | タイムアウト時 | Ctrl-C・SIGTERM・SIGHUP 時 |
|---|---|---|---|
| 制御端末なし（CI） | 専用のグループ | グループ全体を kill | グループ全体を kill |
| 端末あり | smates と同じグループ（ssh のパスフレーズや pinentry をその端末で尋ねられるように） | コマンド自身のプロセスだけを kill | smates と一緒に停止 |

## フォークからの pull request { #fork-pull-requests }

GitHub はフォークからの pull request にシークレットを渡さないため、API キーは使えません。smates は
エージェントなしのオーファンポリシーにフォールバックするだけで、何も失敗しません。

## 失敗の説明 { #failure-explanations }

API キーがあり `--no-agent` でなければ、1 回の実行につき最大 `agent.explain_max` 件（既定 3）の
失敗テストを説明します。`agent.explain_failures: false` で無効にできます。説明には組み込みの
クライアントしか使わないので、`agent.command` を設定した場合も、どのキーがあっても無効になります。説明は失敗ごとにツールなしの 1 リクエストで、上限 30 秒、後続のスイートの実行中にバックグラウンドで
行います。送る内容と返ってくる内容は [失敗ヒント](failure-hints.md#agent-explanations) を参照して
ください。

## 呼び出し元のエージェントに判断させる { #letting-the-calling-agent-decide }

コーディングエージェント（Claude Code、Codex など）が smates を呼ぶ場合、そのエージェントは自分が
何を変更したかを既に知っています。2 つ目の LLM を呼んだりスイート全体を実行したりせずに、オーファンを
自分で判断できます。[コーディングエージェントから使う](coding-agents.md#letting-the-calling-agent-decide)
を参照してください。

## 関連項目 { #see-also }

- [どのテストにも紐づかない変更](orphans.md)
- [smates.yml リファレンス](../reference/configuration.md): `agent.*` のすべてのキー
- [smates が読む環境変数](../reference/environment-variables.md)
