# gh-router 設計

- Status: Draft
- 対象: GitHub.com
- 最終更新: 2026-07-19

## 1. 概要

gh-router は、サンドボックスから GitHub への HTTPS 通信を仲介し、実際の Access Token を proxy 側で付与する Forward Proxy である。

最終的には、request の対象や操作を解釈して細かなポリシーを適用できる仕組みも考えられる。しかし最初からそこまで作り込まず、MVP では次に集中する。

- 実際の GitHub token をサンドボックスへ渡さない。
- 複数 owner 用の Fine-grained personal access token を proxy が使い分ける。
- `gh` と Git over HTTPS を、通常の使い方に近い形で動かす。
- client が持ち込んだ未知の token は GitHub へ転送しない。
- repository と操作の認可は、まず GitHub 上の token scope と permissions に任せる。

MVP に独自の policy engine は持たせない。proxy の route は「どの token を使うか」を決めるためのものであり、「その操作を許可するか」は選ばれた token の scope が決める。

この単純な形を実際の開発環境へ投入し、動かなかった `gh` command や不足した制御を観測してから、target extraction や proxy policy を追加する。

## 2. 最初の利用ケース

MVP は次の環境を対象とする。

- main repository には read/write 用の Fine-grained PAT を用意する。
- 関連する複数 owner の private repository には、owner ごとに read-only の Fine-grained PAT を用意する。
- public repository は、Fine-grained PAT に含まれる public read access を利用して参照する。
- main repository への write と、関連 repository/public repository からの read を同じサンドボックスから行う。
- 同じ proxy instance は、同じ credential set を利用してよいサンドボックス環境だけで共有する。

たとえば次の構成を想定する。

| 対象 | Credential | GitHub 側の scope |
| --- | --- | --- |
| `acme/main` | `main` | 対象 repository、必要な permissions は write |
| `acme-related/*` | `acme-related-read` | 必要な private repositories、permissions は read-only |
| `partner/*` | `partner-read` | 必要な private repositories、permissions は read-only |
| その他の public repository | 明示された default credential | GitHub が全 Fine-grained PAT に与える public read |

main 以外の owner にも write が必要になった場合は、後述の credential hint を明示することで対応できる。ただし MVP の主要シナリオは「main のみ write、他 owner は read」とする。

## 3. MVP の範囲

### 対応する通信

- `api.github.com` の REST API と GraphQL API
- `github.com` の Git smart HTTP
- HTTPS remote を使う `git clone`、`fetch`、`pull`、`push`
- main repository に対する代表的な `gh repo`、`gh pr`、`gh issue` command
- 関連 private repository に対する代表的な read command

REST/GraphQL endpoint ごとの allowlist は作らない。選択された token が許可する API request はそのまま GitHub へ送る。

### MVP では扱わないもの

- SSH または `git://` による Git access
- Git LFS
- release asset の upload/download と CDN redirect
- `raw.githubusercontent.com`、`codeload.github.com` などの追加 host
- Packages、Gist、Codespaces、GitHub Pages、browser session
- GitHub App installation token の生成
- client ごとに異なる policy を持つ shared/multi-tenant proxy
- API operation や branch/ref 単位の独自認可

対応 host や command は、実践投入で必要性を確認してから追加する。

## 4. 全体構成

```mermaid
flowchart LR
    C[Sandbox<br/>gh / git] -->|HTTPS Proxy + dummy token| P[gh-router<br/>TLS termination]
    P --> R[Route selection<br/>Authorization replacement]
    R -->|real token| G[GitHub]
    T[Fine-grained PATs] --> R
```

proxy は一つの process とし、MVP では内部 component を細かく分割しない。request ごとに次だけを行う。

1. 接続先が許可した GitHub host であることを確認する。
2. client の `Authorization` が既知の dummy token、または未指定であることを確認する。
3. credential hint または request の owner/repository から credential を一つ選ぶ。
4. client の認証情報を破棄し、選択した実 token で `Authorization` を作り直す。
5. GitHub へ request を一度だけ転送する。

credential を選べない場合、または client が未知の token/Cookie を送った場合は GitHub へ転送しない。

## 5. 設定

設定として持つものは、listener/CA、credential、route、任意の default credential に絞る。

```yaml
version: 1

server:
  listen: 0.0.0.0:8080
  caCertificate: /etc/gh-router/ca.pem
  caPrivateKey: /etc/gh-router/ca-key.pem

github:
  hosts:
    - api.github.com
    - github.com

routing:
  # gh を認証済みとして動かすが、credential は指定しない dummy token。
  autoHint: gh-router-auto

  # route を抽出できない request に使う credential。省略可能。
  defaultCredential: main

  credentialHints:
    gh-router-main: main
    gh-router-acme-related: acme-related-read
    gh-router-partner: partner-read

  routes:
    - repository: acme/main
      credential: main
    - owner: acme-related
      credential: acme-related-read
    - owner: partner
      credential: partner-read

credentials:
  - id: main
    tokenEnv: GH_ROUTER_TOKEN_MAIN
  - id: acme-related-read
    tokenEnv: GH_ROUTER_TOKEN_ACME_RELATED
  - id: partner-read
    tokenEnv: GH_ROUTER_TOKEN_PARTNER
```

`GH_ROUTER_TOKEN_*` は proxy process にだけ渡す。サンドボックスには渡さない。production の設定ファイルへ token value を直接書かない。

`autoHint` と `credentialHints` の値は GitHub credential ではなく、client が変更できる routing hint である。hint の秘匿性には依存しない。どの hint を選んでも、GitHub で実行できる範囲は対応する Fine-grained PAT の scope を超えない。

### 設定時の確認

- credential ID、hint、exact repository route は重複させない。
- 一つの owner に複数の owner route を定義しない。
- route が参照する credential と `defaultCredential` が存在することを起動時に確認する。
- main token は main repository だけを選択し、必要最小限の write permissions にする。
- related owner の token は必要な repository だけを選択し、read-only permissions にする。
- token には有効期限を設定し、rotation はまず手動運用とする。

route の定義は token scope と一致させるが、MVP では GitHub API を使った scope の自動検証は行わない。

## 6. Credential routing

### 6.1. 選択順序

credential は次の順で決める。

1. `Authorization` に既知の credential hint があれば、その credential を選ぶ。
2. neutral な `autoHint` または認証情報なしの場合は、request から owner/repository を抽出する。
3. exact repository route、owner route の順で探す。
4. route が無ければ、設定された `defaultCredential` を選ぶ。
5. default も無ければ `route_not_found` として拒否する。

credential hint は抽出結果より優先する。hint と実際の対象が合っていなくても proxy は独自認可を行わず、その token で GitHub へ送る。対象が token scope 外なら GitHub が拒否する。

抽出結果が複数 credential を指して一意に決まらない場合は、default を使わず `ambiguous_route` として拒否する。必要なら credential hint で明示する。

一度選択した token で `401`、`403`、`404` になっても別 token は試さない。特に mutation や Git push を自動 retry しない。

owner/repository 名は GitHub の扱いに合わせて case-insensitive に比較し、route lookup では lowercase の canonical form を使う。path の decode や `.git` suffix の除去に失敗した request は default へ流さず拒否する。

### 6.2. REST API

REST API では、代表的な path から routing target を抽出する。

| Path の例 | 抽出する target |
| --- | --- |
| `/repos/{owner}/{repo}/...` | exact repository と owner |
| `/orgs/{owner}/...` | owner |
| `/users/{owner}/...` | owner |

`/user`、`/search/...`、numeric ID だけの endpoint など、owner/repository を抽出できない request は `defaultCredential` または credential hint を使う。

REST operation の read/write 分類や body の解釈は行わない。たとえば repository 作成や transfer が許可されるかは、選択された token の permissions に依存する。

### 6.3. GraphQL API

GraphQL は URL が常に `/graphql` であるため、MVP では JSON body から best-effort で routing target を探す。

- variables 内の `owner` と `repo`/`name` の組
- `repository(owner: ..., name: ...)` の literal
- `organization(login: ...)` または `user(login: ...)` の owner

GraphQL document 全体の意味や query/mutation の認可は行わない。通常の `gh` command で観測した形式だけを小さな extractor として追加する。

node ID しか含まない mutation など、対象を抽出できない request は明示された `defaultCredential` を使う。default を使いたくない場合や別 owner の token が必要な場合は credential hint を指定する。

一つの request に異なる credential route の対象が混在し、hint も無い場合は拒否する。一つの GraphQL request に複数 token を付けたり、query を分割・再実行したりはしない。

### 6.4. Git over HTTPS

Git smart HTTP は次の path から repository を抽出する。

```text
/{owner}/{repo}.git/info/refs
/{owner}/{repo}.git/git-upload-pack
/{owner}/{repo}.git/git-receive-pack
```

read/write の独自判定はせず、upload-pack と receive-pack のどちらにも選択された token を付ける。read-only token で push した場合は GitHub が拒否する。

Git request に client の認証情報が無くてもよい。repository path から route を選び、API では Bearer、Git では Basic auth として実 token を付与する。

## 7. Client setup

サンドボックスでは、薄い wrapper または起動時設定で次を渡す。

```sh
export HTTPS_PROXY='http://gh-router.internal:8080'
export HTTP_PROXY="$HTTPS_PROXY"
export NO_PROXY=''

export SSL_CERT_FILE='/run/gh-router/ca.pem'
export GIT_SSL_CAINFO='/run/gh-router/ca.pem'

export GH_HOST='github.com'
export GH_TOKEN='gh-router-auto'
export GH_PROMPT_DISABLED='1'
```

通常は `gh-router-auto` のまま利用し、proxy の target extraction と default credential に任せる。対象を抽出できない command で credential を指定する場合も、実 token ではなく hint を使う。

```sh
GH_TOKEN='gh-router-partner' gh api graphql ...
```

hint を選ぶ操作は通常の利用では不要にし、実践投入で extractor が対応できなかった command の回避策として残す。後でその command の routing pattern を proxy に追加できる。

Git remote は `https://github.com/OWNER/REPO.git` に統一する。SSH remote を使う既存 repository は、sandbox 構築時に HTTPS へ変換する。

`gh auth token` が返すのは dummy token であり、実 token ではない。これは期待する挙動である。

## 8. MVP に含める安全策

MVP を小さくしても、token の漏洩や持ち込み token の利用に直結する処理は最初から含める。

### TLS と host

- `CONNECT` は設定した GitHub host の port `443` だけ許可する。
- CONNECT authority、TLS SNI、HTTP Host が一致することを確認する。
- proxy の CA 秘密鍵は proxy 側だけに置き、サンドボックスには公開証明書だけを配布する。
- GitHub upstream の TLS certificate を通常どおり検証する。
- 実 token は `api.github.com` と、Git smart HTTP と確認できた `github.com` request にだけ付ける。
- `github.com` では対応する smart HTTP path 以外を拒否し、browser/web request として転送しない。
- redirect は proxy 内で自動追跡しない。MVP の host allowlist 外へ移る処理は失敗させる。

### Client credential

- client の `Authorization` は、未指定、`autoHint`、既知の credential hint だけ許可する。
- `Bearer`/`token` 形式と、Git client が使う Basic auth の password 部分から dummy hint を認識する。
- それ以外の `Authorization`、Cookie、URL userinfo、`access_token`/`client_secret` query parameter は拒否し、単に上書きして転送しない。
- upstream request の `Authorization` は client header を編集するのではなく、選択した token から作り直す。

これにより、侵入者が自分の PAT を設定して自分の repository へ write することを防ぐ。侵入者が既知の hint を選ぶことはできるが、得られるのは設定済み PAT の scope だけである。attacker owner の public repository では public read しか持たないため、write は GitHub に拒否される。

### Secret と log

- 実 token を response、error、access log に含めない。
- `Authorization`、Cookie、proxy CA private key を debug log にも出さない。
- log は request ID、host、抽出した owner/repository、選択した credential ID、status 程度に留める。
- token value の prefix や長さに依存せず、opaque string として扱う。

### この方式の限界

- route は認可ではない。token scope が設定ミスで広ければ、その範囲は利用できてしまう。
- 許可された main repository へ secret を commit、Issue、PR comment として書くことは防がない。
- proxy を利用できる全 sandbox は、全 credential hint を選べるものとして token scope を設計する。
- request smuggling 対策、詳細な parser fuzzing、DLP などを独自実装するものではない。まず成熟した HTTP/TLS library を使い、公開範囲は隔離環境に限定する。
- proxy を経由しない GitHub/外部ネットワークへの通信は、別の egress 制御で遮断する必要がある。

## 9. 実践投入と確認項目

最初は test organization または影響を限定できる開発環境へ配置する。対応を protocol 名だけで判断せず、実際に使う `gh`/`git` command を acceptance test にする。

### 必須シナリオ

1. main repository を `git clone`、`fetch`、`push` できる。
2. main repository で `gh repo view`、`gh pr list/view/create`、`gh issue list/view/create` が動く。
3. `acme-related` と `partner` の private repository を clone/fetch できる。
4. 関連 private repository に `gh repo view`、`gh pr list/view` を実行できる。
5. read-only token を選んだ関連 repository への push/write が GitHub に拒否される。
6. route の無い public OSS repository を clone し、`gh repo view` で参照できる。
7. 対象を抽出できない GraphQL operation を credential hint で正しい token へ送れる。
8. client が独自 PAT、Cookie、URL userinfo を送ると、GitHub へ到達する前に拒否される。
9. attacker 管理 repository への write は、auto/default/各 hint のどれを使っても失敗する。

### 実装時の test

- exact repository route が owner route より優先されること。
- credential hint、auto extraction、default、reject の順序が固定されていること。
- GraphQL で複数 route が見つかった場合に拒否すること。
- fake upstream で client の token/Cookie が一 byte も転送されないこと。
- credential ごとに期待した Bearer/Basic header が付くこと。
- `401`/`403`/`404` の後に別 token を試さないこと。
- allowlist 外 host、CONNECT/SNI/Host 不一致を拒否すること。
- access log と error に token が含まれないこと。

投入後は `route_not_found`、`ambiguous_route`、default 使用回数と失敗した command を記録する。実際に必要になった pattern だけ extractor または route に追加する。

## 10. 将来の方向性

MVP の運用結果を基に、必要なものを個別に追加する。

- **Routing 改善**: よく使う REST path、GraphQL variables/literal、numeric/node ID の resolver を増やす。
- **Proxy policy**: token scope だけでは広すぎることが確認された操作に、repository/action 単位の deny/allow を追加する。
- **Credential provider**: GitHub App installation token、短命 token、secret store、rotation 自動化へ対応する。
- **Shared proxy**: 異なる権限の sandbox を同じ instance で扱う必要が出た場合に、client authentication と profile を追加する。
- **追加 protocol/host**: Git LFS、release asset、raw/codeload、Packages へ必要な範囲で対応する。
- **運用機能**: metrics、alert、config reload、HA、より強い parser validation を利用規模に合わせて追加する。
- **互換性管理**: 利用する `gh` version と command matrix を CI で継続確認する。

最終的に細かな proxy policy が必要になっても、MVP の `credential hint -> target extraction -> explicit default` という routing は credential 選択の仕組みとして残せる。policy は routing の前段に一度に作るのではなく、token scope だけでは防げない具体的な操作が見つかった時点で追加する。

## 11. 参考資料

- [Managing your personal access tokens - GitHub Docs](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens): Fine-grained PAT の resource owner、repository access、public repository read
- [GitHub CLI environment variables](https://cli.github.com/manual/gh_help_environment): `GH_TOKEN`、`GH_HOST`、`GH_REPO`
- [Authenticating to the REST API - GitHub Docs](https://docs.github.com/en/rest/authentication/authenticating-to-the-rest-api): REST API の token authentication
- [Forming calls with GraphQL - GitHub Docs](https://docs.github.com/en/graphql/guides/forming-calls-with-graphql): GraphQL endpoint、variables、query/mutation
- [Git HTTP protocol](https://git-scm.com/docs/http-protocol): `info/refs`、`git-upload-pack`、`git-receive-pack`
