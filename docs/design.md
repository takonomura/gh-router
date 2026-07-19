# gh-router 設計

- Status: Draft
- 対象: GitHub.com
- 最終更新: 2026-07-19

## 1. 概要

gh-router は、サンドボックスから GitHub への HTTPS 通信を仲介する Forward Proxy である。サンドボックスには実際の GitHub credential を渡さず、プロキシがリクエストの対象と操作を検査し、許可された場合だけ適切な credential を付与して GitHub へ転送する。

この仕組みの中心は、単なる「URL ごとのトークン切り替え」ではなく、次の二段階の認可である。

1. プロキシ自身のポリシーで、client、対象 resource、操作を認可する。
2. 認可済みの操作に必要な最小権限の credential を選び、client 由来の認証情報を置換して送信する。

credential が GitHub 上で持つ権限だけに依存すると、owner 単位のトークンを選んだ後に、その owner 内の意図しない repository や管理系 API を呼べてしまう可能性がある。したがって、プロキシのポリシーは credential の権限とは独立に、かつ credential より狭く保つ。

初期リリースでは、対象 repository が URL から明確に決まる REST API と Git over HTTPS を優先する。GraphQL は単一の `/graphql` endpoint に異なる resource への操作が混在し、mutation が opaque な node ID だけを受け取ることもあるため、別の protocol adapter として段階的に対応する。

## 2. 背景と目的

開発環境、特に Coding Agent を実行するサンドボックスでは、侵害を前提として次を両立したい。

- 作業対象 repository では、clone、fetch、push、Pull Request や Issue の操作など、開発に必要な read/write を許可する。
- 関連する organization の repository では read のみ許可する。
- 複数 owner にまたがる private repository を扱えるようにする。
- public repository の read は owner を事前列挙せず許可する。
- `gh` や `git` に複数 credential の存在を意識させない。
- サンドボックス内に実際の GitHub credential を置かない。
- 侵入者が自分の GitHub token や Cookie を持ち込み、自分の管理下の repository、Issue、Gist などへ情報を書き出すことを防ぐ。

Fine-grained personal access token は一つの resource owner に紐づき、選択した repository と permissions に絞れる一方、複数 owner にまたがる場合は複数 token が必要になる。また、fine-grained token は GitHub 上の全 public repository への read-only access を常に含む。これらの性質は credential の発行には有用だが、client 側での token 選択問題は解決しないため、gh-router で吸収する。

## 3. 要件

### 3.1. 機能要件

| ID | 要件 |
| --- | --- |
| F-1 | client ごとに異なる access policy を適用できること |
| F-2 | exact repository、owner 配下、public repository という単位で対象を指定できること |
| F-3 | read/write だけでなく、contents、issues、pull requests、actions などの capability 単位で制御できること |
| F-4 | 複数の PAT または GitHub App installation credential を保持し、決定的に使い分けられること |
| F-5 | REST API、GraphQL API、Git smart HTTP を独立した classifier で扱えること |
| F-6 | 未対応の host、endpoint、GraphQL field/mutation、Git service は deny すること |
| F-7 | `gh` と `git` の client 変更を、環境変数、CA 証明書、必要最小限の wrapper 設定に留めること |
| F-8 | allow/deny と credential 選択結果を secret を含めず監査できること |

### 3.2. Security invariant

実装は少なくとも次を常に満たす。

1. **Default deny**: 分類不能、曖昧、未知の request は GitHub へ送信しない。
2. **Authorize before credential selection**: 対象と操作の認可が終わるまで upstream credential を選ばない。
3. **No client credential forwarding**: client 由来の `Authorization`、`Cookie`、URL userinfo などを upstream へ転送しない。
4. **Reject unknown credential**: client が送った未知の token を黙って上書きせず、request 自体を拒否する。
5. **No credential exposure**: upstream credential は response、error、log、metric、trace に含めない。
6. **Least privilege routing**: 匿名で可能なら匿名、次に exact repository 用、owner 用の順で、操作を満たす最小の credential を一つだけ選ぶ。
7. **No privileged fallback**: 選んだ token で `401`、`403` になっても、より強い token を順番に試さない。mutation の retry もしない。
8. **Trusted-origin only**: GitHub credential を付与できる upstream host を固定し、redirect/CDN host には付与しない。
9. **Policy is narrower than credential**: proxy policy の許可範囲が GitHub credential 自体の scope を超えないことを設定検証する。
10. **Fail closed**: classifier、secret store、credential mint、policy reload に失敗した場合は、古い credential や別 credential へ迂回せず拒否する。

### 3.3. 前提

- サンドボックスから GitHub または外部ネットワークへ、gh-router を経由しない経路は別レイヤーで遮断される。
- `github.com:22`、`ssh.github.com:443`、`git://` は遮断し、Git 操作には HTTPS remote を使う。
- proxy host と secret store はサンドボックスより強い trust domain に置く。
- サンドボックスは proxy 用の client credential とダミー token を読み取れる。これらは GitHub credential ではなく、そのサンドボックスに割り当てられた policy を利用するための識別子である。
- GitHub Enterprise Server は初期 scope 外とし、必要になった時点で host、API version、schema、credential issuer を別設定として追加する。

### 3.4. 非目標と限界

- gh-router は汎用 DLP ではない。許可された作業 repository への write が必要である以上、侵入者はその repository の commit、Issue、Pull Request comment などへ情報を書ける。これを防ぐには content scanning、secret scanning、review、branch protection など別の対策が必要である。
- 許可された private repository から読み取った情報を、ローカルの標準出力やファイルへ保存することは防がない。
- Git commit の author、GitHub 上の actor、監査ログ上の主体を元の利用者と一致させることは自動では保証しない。使用した PAT の user または GitHub App bot が actor になる。
- repository 単位の Git push 許可だけでは branch/ref 単位の制御にならない。初期実装では GitHub ruleset、branch protection、token permissions に委ねる。
- proxy 自身または proxy の CA 秘密鍵、secret store が侵害された場合は本設計の防御範囲外である。

## 4. Architecture

```mermaid
flowchart LR
    C[Sandbox<br/>gh / git / curl] -->|CONNECT + TLS| P[Forward Proxy<br/>TLS termination]
    P --> N[Request normalization<br/>credential sanitization]
    N --> A[Protocol classifier<br/>target/action extraction]
    A --> E[Policy engine]
    E --> R[Credential router]
    R --> U[Upstream transport]
    U --> G[GitHub]

    CFG[Versioned policy/config] --> E
    CFG --> A
    S[Secret store / GitHub App key] --> R
    E --> L[Audit log / metrics]
    R --> L
```

論理 component は次の責務を持つ。

| Component | 責務 |
| --- | --- |
| Listener / TLS terminator | `CONNECT` の client 認証、許可 host の検査、動的 server certificate の発行、TLS termination |
| Normalizer | authority、SNI、Host、path、header、body framing の正規化と曖昧な request の拒否 |
| Credential sanitizer | client 由来 credential の検査・除去、hop-by-hop header の除去 |
| Protocol classifier | REST、GraphQL、Git、download の識別と canonical target/action の抽出 |
| Policy engine | principal、target、action に対する allow/deny 判定 |
| Credential router | allow rule と credential constraints から最小権限 credential を一つ選択 |
| Credential provider | secret の取得、GitHub App installation token の mint と期限管理 |
| Upstream transport | GitHub の certificate 検証、認証 header の再構築、redirect を追わない転送 |
| Audit/metrics | secret を除いた decision log、GitHub request ID、rate limit、latency の記録 |

実装を一つの process にまとめることは可能だが、component 間では `principal + canonical request -> target + action -> decision + credential id` という境界を保ち、classifier と policy engine を HTTP forwarding code から分離する。

## 5. TLS Forward Proxy と client の接続

### 5.1. CONNECT と MITM

1. client は `CONNECT api.github.com:443` のように proxy へ接続する。
2. proxy は `Proxy-Authorization` などで client を認証し、`principal` に変換する。失敗時は `407 Proxy Authentication Required` を返す。
3. CONNECT authority が allowlist の host と port `443` であることを確認する。
4. proxy は専用 CA で対象 host の leaf certificate を発行し、client との TLS を終端する。
5. TLS SNI、CONNECT authority、HTTP `Host`/`:authority` が同一の正規化済み host であることを確認する。
6. proxy は system trust store を使って GitHub upstream の TLS certificate を通常どおり検証する。

CA の公開証明書だけをサンドボックスへ配布し、CA 秘密鍵は proxy 内に保持する。この CA は GitHub allowlist の leaf certificate 発行にのみ使い、一般端末や他用途へインストールしない。HTTP/1.1 を初期対応とし、downstream ALPN で `http/1.1` のみ提示する。HTTP/2 対応時も同じ正規化・分類を通し、extended CONNECT や protocol upgrade は個別に許可するまで deny する。

平文 HTTP の GitHub origin request や、CONNECT 後に TLS を使わない通信は deny する。proxy までの hop 自体を平文 HTTP proxy protocol にする場合も、隔離ネットワーク外では client token が露出しないよう proxy TLS または mTLS を使う。

### 5.2. client wrapper

wrapper は概念的に次を設定する。値は例であり、実際には sandbox ごとに発行する。

```sh
export HTTPS_PROXY='http://sandbox-a:PROXY_CLIENT_TOKEN@gh-router.internal:8080'
export HTTP_PROXY="$HTTPS_PROXY"
export NO_PROXY=''

export SSL_CERT_FILE='/run/gh-router/ca.pem'
export GIT_SSL_CAINFO='/run/gh-router/ca.pem'

export GH_HOST='github.com'
export GH_TOKEN='gh-router-placeholder-sandbox-a'
export GH_PROMPT_DISABLED='1'
```

`GH_TOKEN` は `gh` に login 済みと認識させるための無効な marker であり、proxy が exact match で認識して除去する。GitHub へ直接送っても利用できない値にする。`git` は origin の認証 header なしでもよく、proxy 認証済み principal と repository URL から credential を注入できる。credential helper や `gh` が marker を Basic/Bearer header として送る場合は、設定済み marker と一致する形式だけ受理する。

この構成では `gh auth token` が出力できるのは実 token ではなく marker であり、これは期待する安全側の挙動である。`gh auth status` など認証状態そのものを調べる command の表示は通常環境と異なる可能性があるため、command compatibility の対象として個別に定義する。

`HTTPS_PROXY` の client token は GitHub token ではないものの、別 sandbox の policy を利用されないよう sandbox ごとに分け、hash のみ設定に保存する。proxy までのネットワークが共有される場合は、proxy 自体への TLS または mTLS も検討する。

Git remote は `https://github.com/OWNER/REPO.git` に統一する。SSH 形式を利用する既存 repository には、sandbox 構築時に HTTPS への `insteadOf` 設定を行うことができる。

環境変数や CA を sandbox 内で変更されても、proxy 外への egress が閉じていれば権限は増えない。CA を信頼しなくした場合は通信が失敗するだけである。

## 6. Request processing

すべての request を次の順序で処理する。

1. **Client authentication**: CONNECT 時の認証から principal を確定する。
2. **Origin validation**: CONNECT authority、SNI、Host、port、scheme を照合する。
3. **Canonicalization**: host の case、trailing dot、IDNA、path、percent encoding、query、header framing を正規化する。
4. **Client credential check**: 許可されたダミー marker、または credential なしだけを受理する。未知の認証情報があれば reject する。
5. **Protocol classification**: host、method、path、query、Content-Type、必要な範囲の body から operation、target、action を抽出する。
6. **Policy decision**: principal に対して target/action が明示的に許可されているか確認する。
7. **Credential selection**: 許可 rule と credential constraints を満たす最小権限 credential を一つ選ぶ。
8. **Upstream request rebuild**: client の認証情報を破棄し、Host と認証 header をゼロから構築する。
9. **Forward once**: redirect を内部で追わず、mutation を自動 retry せず、GitHub へ一度だけ送る。
10. **Audit**: decision、credential ID、GitHub request ID、結果を secret/body なしで記録する。

### 6.1. Normalization と request smuggling 対策

少なくとも次を reject する。

- CONNECT authority、TLS SNI、HTTP authority の不一致
- IP address literal、allowlist 外 port、userinfo 付き URL
- encoded slash/backslash、NUL、二重 percent encoding、path traversal、曖昧な `//` を含む path
- 重複または矛盾する `Host`、`Content-Length`、`Transfer-Encoding`
- absolute-form request target の authority と Host の不一致
- endpoint classifier が inspect する body の未知の `Content-Encoding`
- protocol upgrade、WebSocket、未知の CONNECT protocol
- classifier ごとの上限を超える header、GraphQL JSON、JSON request body

Go や利用する HTTP library が行う正規化だけを前提にせず、policy lookup に使った canonical representation と upstream へ送る representation が同じ意味になることを test する。

### 6.2. Client credential の扱い

次を client credential として扱う。

- `Authorization`
- `Cookie`
- URL userinfo
- `access_token`、`client_secret` など認証用途として知られた query parameter
- credential 発行・交換 endpoint への request

`Authorization` は、未指定か、設定済み marker の Bearer/Basic 表現との exact match のみ許可する。それ以外は `403 client_credential_rejected` とし、上書きして続行しない。`Cookie` と URL userinfo は常に拒否する。OAuth token 交換、PAT 管理、SSH/GPG key 登録、deploy key、repository secret、webhook など credential や外部送信経路を作る operation は、専用 capability が将来追加されるまで deny する。

upstream へは header allowlist を基に再構築し、少なくとも `Authorization`、`Proxy-Authorization`、`Cookie`、`Forwarded`、`X-Forwarded-*` と hop-by-hop header を client から引き継がない。

## 7. Policy model

### 7.1. Decision input

policy engine への入力を次の canonical tuple とする。

```text
(principal, protocol, operation, target, action, request attributes)
```

- `protocol`: `rest`、`graphql`、`git`、`download`
- `operation`: version 固定された REST operation ID、GraphQL root field/mutation、Git service など
- `target`: `repository(owner/name)`、`owner(name)`、`account(self)`、`public-global` など
- `action`: 下記 capability
- `request attributes`: method、visibility、body から抽出した secondary target など

### 7.2. Capability

「repository write」を一つの巨大な権限にせず、少なくとも次へ分割する。

```text
repo.metadata.read
repo.contents.read
repo.contents.write
repo.issues.read
repo.issues.write
repo.pull_requests.read
repo.pull_requests.write
repo.checks.read
repo.checks.write
repo.actions.read
repo.actions.write
repo.releases.read
repo.releases.write
repo.administration.read
repo.administration.write
repo.credentials.write
org.metadata.read
org.administration.write
account.self.read
public.search.read
```

`repo.contents.write` は Git push や Contents API を含むが、repository transfer、delete、deploy key、Actions secret、webhook 作成を含めない。これらは `repo.administration.write` または `repo.credentials.write` とし、通常の Coding Agent policy では明示的に deny する。

proxy capability と GitHub token permission は同じものではない。各 protocol operation に `requiredGitHubPermissions` を持たせ、policy の capability 認可後に credential が GitHub 側の必要 permission を満たすか検証する。複数 permission の AND/OR 条件も表現し、単純な `issues.write -> issues:write` の一対一対応を前提にしない。

HTTP method だけから action を決めない。たとえば REST の `POST /repos/{owner}/{repo}/forks` は source repository への単純な write ではなく、body の `organization` に新しい repository を作る cross-target operation である。source と destination の両方を安全に認可できない operation は deny する。

### 7.3. Target match と競合解決

repository target は大文字小文字を正規化した `owner/name` で比較し、次の match を提供する。

- exact repository: `acme/main`
- owner 配下: `acme-related/*`
- visibility: `public`
- 明示的な repository set

明示 deny は常に allow より優先する。同じ request に複数 allow rule が match し、異なる credential route や異なる意味になる場合は、実行時の rule order に頼らず config compile 時に曖昧として拒否する。public な exact repository の read のように複数 credential が安全に使える場合は、匿名を最優先する deterministic selection rule を定義する。

repository rename/transfer endpoint 自体は deny する。GitHub が旧 URL から新 URL へ redirect した場合も proxy 内では追わず、client からの次 request を新しい target として再認可する。numeric repository ID や GraphQL node ID による target は、信頼できる resolver で canonical owner/name に解決できる場合だけ許可する。

### 7.4. 設定例

以下は設定 model の例であり、最終 schema ではない。secret 本体は設定ファイルへ書かない。

```yaml
apiVersion: gh-router/v1alpha1

server:
  listen: 0.0.0.0:8080
  mitmCA:
    certificate: /etc/gh-router/ca.pem
    privateKeySecretRef: secret://gh-router/mitm-ca-key

github:
  restApiVersion: "2026-03-10"
  endpointCatalog:
    openapiRevision: "PINNED_GIT_COMMIT"
    overlay: /etc/gh-router/github-operations.yaml
  hosts:
    api.github.com: [rest, graphql]
    github.com: [git]
    uploads.github.com: [rest-upload]

clients:
  - id: coding-agent
    proxyAuthHash: "argon2id:..."
    originMarkerHash: "sha256:..."
    policy: coding-agent-default
    identityCredential: main-repo

credentials:
  - id: anonymous
    kind: anonymous

  - id: public-graphql
    kind: token
    secretRef: secret://github/public-reader
    constraints:
      publicOnly: true
      permissions: [metadata:read, contents:read, issues:read, pull_requests:read]

  - id: main-repo
    kind: githubAppInstallation
    appPrivateKeySecretRef: secret://github/gh-router-app-key
    installationId: 10001
    mint:
      repositories: [acme/main]
      permissions:
        metadata: read
        contents: write
        issues: write
        pull_requests: write
        checks: write

  - id: related-org-read
    kind: fineGrainedPAT
    secretRef: secret://github/acme-related-read
    constraints:
      owners: [acme-related]
      permissions:
        metadata: read
        contents: read
        issues: read
        pull_requests: read

policies:
  - id: coding-agent-default
    explicitDeny:
      - repo.administration.write
      - repo.credentials.write
      - org.administration.write

    repositoryRules:
      - match:
          repositories: [acme/main]
        allow:
          - repo.metadata.read
          - repo.contents.read
          - repo.contents.write
          - repo.issues.read
          - repo.issues.write
          - repo.pull_requests.read
          - repo.pull_requests.write
          - repo.checks.read
          - repo.checks.write
        credentials: [main-repo]

      - match:
          owners: [acme-related]
        allow:
          - repo.metadata.read
          - repo.contents.read
          - repo.issues.read
          - repo.pull_requests.read
        credentials: [related-org-read]

      - match:
          visibility: public
        allow:
          - repo.metadata.read
          - repo.contents.read
          - repo.issues.read
          - repo.pull_requests.read
          - repo.releases.read
        credentials:
          rest: anonymous
          git: anonymous
          graphql: public-graphql

    globalRules:
      - target: public-global
        allow: [public.search.read]
        credentials: [public-graphql]
```

`publicOnly` は PAT から完全に機械検証できるとは限らないため、専用 machine user で発行し、private repository への access をその user に与えない。さらに proxy は GraphQL で明示された repository が public であることを匿名 REST lookup で確認し、短い TTL で cache する。匿名で実行できる REST/Git read は public-reader token を付けず、その request が private なら GitHub の `404` をそのまま返す。

### 7.5. Config validation

起動・reload 時に次を検証する。

- allow rule の capability を満たす credential が存在する。
- credential の宣言 owner/repository/permission が rule より狭すぎたり、想定外に広すぎたりしない。
- public wildcard に write capability がない。
- exact/owner/public rule 間の credential selection が一意である。
- REST API version と endpoint catalog/OpenAPI revision が対応している。
- GitHub App installation の owner、repository selection、permissions を API で確認できる。
- PAT の scope を完全確認できない項目は明示的な operator assertion とし、警告ではなく production policy に応じて起動失敗にできる。
- inline secret を production mode では禁止する。

reload は新設定を全検証してから atomic に切り替える。新設定が不正なら現在の正常な設定を維持し、監査 event を出す。ただし期限切れ credential を古い設定のまま利用し続けることはしない。

## 8. Credential model と選択

### 8.1. Credential type

| Type | 用途 | 特徴 |
| --- | --- | --- |
| Anonymous | public REST、public Git fetch | secret がなく最小権限。rate limit は低い |
| Fine-grained PAT | user attribution が必要、GitHub App を導入できない owner | resource owner ごとに必要。期限・approval・rotation を運用する |
| GitHub App installation | organization 管理、bot attribution、短命 token | 推奨。installation は owner ごとだが、proxy が違いを隠蔽できる |
| Classic PAT | fine-grained/App 非対応 endpoint の一時的互換 | scope が広くなりやすいため原則避け、例外を operation allowlist で囲う |

GitHub App installation token は、installation に許可された範囲内で mint 時に repository と permissions をさらに絞れ、1 時間で失効する。proxy は `(installation, repository set, permissions)` ごとに token を mint し、失効より十分前に破棄する。App private key は最も強い credential になるため secret store/HSM で管理し、sandbox へ出さない。

Fine-grained PAT を使う場合も、main repository 専用 write token と owner read token を分ける。単一の owner-wide write token を proxy policy だけで狭めるより、GitHub 側の resource scope も合わせて狭める。

### 8.2. Selection algorithm

認可済み request に対し、概念的に次を行う。

```text
candidates = credentials referenced by the matched allow rule
candidates = filter(candidates, supports(protocol, target, action))

if request can be served anonymously and target is public:
    select anonymous
else:
    select the unique least-privileged candidate

if no unique candidate exists:
    deny
```

選択は request を GitHub へ送る前に完了させる。`404` や `403` を受けて別 token を試す方式は、private resource の存在確認、actor の不整合、rate limit 消費、mutation の二重実行につながるため採用しない。

### 8.3. Upstream authentication

- REST/GraphQL: proxy が `Authorization: Bearer <token>` を構築する。
- Git over HTTPS: proxy が `Authorization: Basic base64(x-access-token:<token>)` を構築する。PAT では username は認証判断に使われないが、形式を統一する。
- Anonymous: `Authorization` を送らない。

token の値や prefix/長さに依存した判定をしない。token format は変更され得るため、opaque secret として扱う。

## 9. Protocol adapter

### 9.1. REST API

REST は GitHub 公式の OpenAPI description を pin して path template と operation ID の基礎に使い、gh-router 固有 overlay で次を付与する。

```yaml
operation: issues/create
method: POST
path: /repos/{owner}/{repo}/issues
targetExtractor: repositoryFromPath(owner, repo)
action: repo.issues.write
secondaryTargetExtractor: none
credentialClass: repository
requiredGitHubPermissions:
  anyOf:
    - {issues: write}
```

OpenAPI だけでは security semantics は十分でないため、method から read/write を自動推論して許可しない。すべての対応 operation に target extractor、action、GitHub permission 条件を review 済み overlay として持つ。permission 条件は公式 endpoint documentation と `X-Accepted-GitHub-Permissions` を基に test する。GitHub REST API version は明示的に pin し、client header が未指定なら挿入、異なる version なら拒否する。version を上げるときは OpenAPI revision、overlay、compatibility test を同時に更新する。設定例の `2026-03-10` は設計時点の最新版だが、実運用では対応する `gh` version と検証済みの API version を選び、自動で最新版へ追従しない。

代表的な分類は次のとおり。

| Request | Target | Action |
| --- | --- | --- |
| `GET /repos/acme/main` | `acme/main` | `repo.metadata.read` |
| `GET /repos/acme/main/contents/...` | `acme/main` | `repo.contents.read` |
| `PUT /repos/acme/main/contents/...` | `acme/main` | `repo.contents.write` |
| `POST /repos/acme/main/issues` | `acme/main` | `repo.issues.write` |
| `POST /repos/acme/main/pulls` | `acme/main` | `repo.pull_requests.write` |
| `POST /repos/acme/main/forks` | source と body 内 destination | 初期 deny |
| `POST /orgs/acme/repos` | owner と新規 repository | 初期 deny |
| `PATCH /repos/acme/main` で rename/transfer | 複数 target | deny |
| `GET /repositories/{id}` | resolver が返した owner/name | resolver 未対応なら deny |
| `GET /user` | `account(self)` | 明示した identity credential でのみ許可 |

`gh api` は任意の HTTP tunnel にはならず、catalog にある operation だけ利用できる。preview media type や新 endpoint は、overlay を追加して review するまで deny する。

### 9.2. GraphQL API

GraphQL は常に `api.github.com/graphql` を使い、query も mutation も通常 `POST` である。URL だけでは target も read/write も決められないため、REST と同じ forwarding handler で method だけを見る実装は安全ではない。

GraphQL adapter は次を行う。

1. JSON body、`operationName`、variables、GraphQL document を parse する。
2. fragment、alias、directive、複数 root field を展開した semantic tree を作る。
3. root field/mutation ごとの registry から action、target extractor、必要な GitHub permission を得る。
4. `repository(owner:, name:)` の literal/variable を canonical repository に解決する。
5. document に現れるすべての target/action を認可する。
6. request 全体を一つの credential で安全に処理できる場合だけ転送する。

一つの query が異なる owner の private repository を同時に読む場合、単一の GitHub request に複数 token は付けられない。query の分割と response 再合成は alias、error、pagination、rate limit semantics を変えるため、初期設計では行わず `graphql_mixed_scope` として deny する。

さらに mutation は `subjectId` や `pullRequestId` のような opaque global node ID だけで対象を指定することがある。schema introspection だけではその ID がどの repository に属するか判断できない。このため段階的に次を採る。

- Phase 1: GraphQL を deny する。
- Phase 2: 単一の明示的 `repository(owner:, name:)` に閉じた read query と、`viewer.login` のような安全な scalar field の allowlist に対応する。`viewer.repositories` のように別 target へ展開する subtree は root 名だけで許可しない。
- Phase 3: 許可済み query response から、ownership を証明できる node ID と repository の対応を server-side provenance cache に保存する。全 ID を解決できる allowlisted mutation だけ対応する。
- 未知 field、`node`/`nodes`、`resource(url:)`、private search、provenance のない ID、複数 operation document、persisted query は個別対応まで deny する。

provenance cache は client の申告を信用せず、proxy が認可済み GitHub response から作る。短い TTL と config revision を持たせ、rename/transfer、`404`、redirect を観測した mapping は破棄する。cache miss を別 token の総当たりで解決しない。必要であれば、operation ごとに安全な read-only resolver を設計する。

この制約により、GraphQL を内部利用する `gh pr` などは初期版では動作しない可能性がある。サポート単位を「REST 対応済み」ではなく、後述する `gh` command compatibility matrix とする。

### 9.3. Git smart HTTP

Git HTTPS URL は path から repository が一意に決まり、初期対応に向いている。smart HTTP の service を次へ分類する。

| Request | Action |
| --- | --- |
| `GET /OWNER/REPO.git/info/refs?service=git-upload-pack` | `repo.contents.read` |
| `POST /OWNER/REPO.git/git-upload-pack` | `repo.contents.read` |
| `GET /OWNER/REPO.git/info/refs?service=git-receive-pack` | `repo.contents.write` |
| `POST /OWNER/REPO.git/git-receive-pack` | `repo.contents.write` |

path、query parameter、method、Content-Type の組を厳密に検査し、未知 service や dumb HTTP fallback は必要性を確認するまで deny する。Git protocol v2 も同じ endpoint を使うため、許可した `Git-Protocol` header の値を upstream へ渡す。

repository が public で read の場合は匿名、private read または write は repository/owner に対応する credential を Basic auth として付与する。receive-pack body を解析しなくても repository 単位の write は制御できるが、ref 単位の制御が必要なら pkt-line parser と全 command の検証を別機能として追加する。それまでは GitHub ruleset/branch protection を必須の defense-in-depth とする。

credential が GitHub App token の場合、upload-pack には Contents read、receive-pack には Contents write permission を要求する。PAT についても同等の実効権限を credential constraints に宣言する。

submodule、別 remote、fork への fetch/push はそれぞれ別 HTTP request になるため、各 repository を同じ policy で個別に判定する。

### 9.4. Raw content、archive、release asset、Git LFS

`raw.githubusercontent.com`、`codeload.github.com`、release asset の CDN、Git LFS transfer URL は host と URL 形式が異なる。GitHub token をこれらへ一律に付けることはしない。

- owner/repository を path から確実に抽出できる read host は専用 classifier を実装する。
- API または Git response の redirect 先は proxy が自動 follow しない。
- 認可済み response から得た署名付き download URL は、exact URL hash、principal、method、expiry を紐づけた短命 download lease として登録できる。
- lease 経由 request では GitHub Authorization を付けず、署名付き URL 自体だけを利用する。
- LFS batch response 内の download/upload action は JSON response の解析が必要である。read lease と write lease を分け、upload は元の repository に `repo.contents.write` がある場合だけ許可する。
- Packages、Codespaces、Gist、GitHub Pages、browser UI は、host を列挙して semantic classifier を追加するまで scope 外とする。

## 10. Host policy

初期 allowlist は広い wildcard ではなく、host と protocol の組で持つ。

| Host | 初期用途 | Credential injection |
| --- | --- | --- |
| `api.github.com` | REST、後に GraphQL | 選択済み Bearer token のみ |
| `github.com` | Git smart HTTP の限定 path | 選択済み Basic token のみ |
| `uploads.github.com` | 対応済み release upload endpoint | 選択済み Bearer token のみ |
| GitHub CDN/object host | 登録済み download/LFS lease | なし |

`*.github.com` や `*.githubusercontent.com` の一括 allow は行わない。DNS 解決後の IP ではなく canonical host で policy を適用し、upstream TLS certificate でも host identity を検証する。GitHub の IP range 更新を host authorization の代替にしない。

## 11. Error handling

proxy 生成 error は request ID と安定した machine-readable code を返し、credential 名や policy の詳細を client に出しすぎない。

| 状況 | Status/code の例 |
| --- | --- |
| proxy client 認証失敗 | `407 proxy_auth_required` |
| 未知の client token/Cookie | `403 client_credential_rejected` |
| host 不許可 | `403 host_denied` |
| endpoint/GraphQL operation 未対応 | `403 operation_unknown` |
| target/action 不許可 | `403 policy_denied` |
| 複数 scope の GraphQL | `403 graphql_mixed_scope` |
| credential 取得不能・期限切れ | `503 credential_unavailable` |
| request の曖昧性・不正 framing | `400 malformed_request` |

GitHub 由来の `401`、`403`、`404`、rate limit response は、secret を含む header を除いてそのまま返し、別 credential へ fallback しない。

## 12. Audit、monitoring、運用

### 12.1. Audit log

各 request について次を構造化記録する。

- timestamp、request ID、principal
- canonical host、protocol、operation ID
- target owner/repository、action
- allow/deny と reason code、matched policy/rule ID
- credential ID と kind。secret/value/fingerprint は記録しない
- upstream status、GitHub request ID、rate-limit headers
- request/response bytes、latency

body、GraphQL variables、query string、Git ref payload は標準では記録しない。signed URL、`Authorization`、`Proxy-Authorization`、Cookie、token らしい値は log pipeline の全段で redact する。debug mode でも secret header の出力を禁止する。`GH_DEBUG=api` を client が有効にするとダミー marker や request body が client 側 log に出る可能性があるため、sandbox の成果物収集方針でも扱う。

### 12.2. Metrics と alert

- decision 数を protocol/action/allow-deny/reason で集計する。
- unknown operation、unknown credential、host mismatch、GraphQL parse failure の急増を alert する。
- credential ごとの GitHub rate limit 残量と installation token mint failure を監視する。
- public anonymous traffic と authenticated traffic を分ける。
- config revision、classifier revision、対応 `gh` version を build info として公開する。

### 12.3. Credential lifecycle

- static PAT は期限を必須とし、期限前 alert と重複期間を設けて atomic に rotation する。
- GitHub App token は短命 cache とし、同一 scope の同時 mint を single-flight する。
- secret 取得不能時に config 内へ fallback token を置かない。
- revoke 手順、App installation suspend、CA rotation、client credential revoke を runbook 化する。
- actor attribution と organization audit log を定期的に照合する。

## 13. 段階的な実装計画

### Phase 0: Security skeleton

- CONNECT client 認証、host/SNI/authority 検査
- GitHub host 限定の TLS termination
- client credential reject/strip と upstream header 再構築
- versioned config、default deny、decision audit
- fake upstream を使った「未知 token が一 byte も upstream へ出ない」test

### Phase 1: REST subset + Git

- exact repository と owner rule
- public REST/Git read の匿名転送
- `git clone/fetch/push` の smart HTTP
- よく使う `/repos/{owner}/{repo}/...` REST read と、main repository の厳選した write operation
- PAT provider と GitHub App installation provider
- GraphQL、LFS、redirect lease は明示 deny

最初の acceptance scenario は次とする。

1. public OSS repository を匿名 clone/fetch できる。
2. related owner の private repository を clone/fetch でき、push は拒否される。
3. main repository を clone/fetch/push できる。
4. attacker owner の repository は、client が独自 PAT、marker、認証なしのどれを使っても write できない。
5. main write token が GitHub 上でアクセス可能な別 repository でも、proxy policy に無ければ deny される。

### Phase 2: `gh` read compatibility

- 単一 repository scope の GraphQL read
- `viewer`/identity の限定対応
- public search 用の隔離した public-reader
- raw content、archive、release download lease
- 対応する `gh` command/version の compatibility matrix と CI

候補 command は `gh repo view`、`gh pr list/view`、`gh issue list/view`、`gh run list/view` から始める。各 command が REST/GraphQL のどの operation を使うかは `gh` version で変わり得るため、protocol 対応ではなく end-to-end test を契約とする。

### Phase 3: GraphQL mutation + extended transfer

- node ID provenance store と operation-specific resolver
- `gh pr create/comment/review`、`gh issue create/comment` などの mutation allowlist
- release upload、Git LFS read/write lease
- 必要なら Git receive-pack の ref command inspection

### Phase 4: Hardening / production

- HA、secret store/HSM、CA rotation
- policy dry-run を trusted staging traffic に限定して導入
- parser fuzzing、request smuggling test、load/rate-limit test
- GitHub API version と `gh` upgrade の定期 review

production sandbox で unknown operation を audit-only allow する mode は設けない。新しい `gh` behavior の観測は credential を持たない capture、fake upstream、または隔離した staging policy で行う。

## 14. Test strategy

### 14.1. Unit / table test

- URL canonicalization、case、percent encoding、`.git` suffix、query parameter
- REST operation -> target/action mapping の全件 snapshot
- Git service -> read/write mapping
- GraphQL fragment/alias/variables/multiple operation の展開
- policy conflict と credential least-privilege selection

### 14.2. Property / fuzz test

次を property とする。

- 未知 operation は常に deny される。
- client 由来認証情報が upstream request に残らない。
- upstream token が credential injection allowlist 外 host に付かない。
- public wildcard rule から write decision が生成されない。
- write decision の target は必ず明示 allow rule に含まれる。
- canonicalization 前後で classifier の意味が変わる request は reject される。
- GraphQL の全 root field/ID を解決できない限り allow されない。

HTTP request smuggling、duplicate header、absolute URI、CONNECT/SNI/Host mismatch、compressed body、巨大 GraphQL document、deep fragment、redirect chain を fuzz corpus に含める。

### 14.3. Integration / compatibility test

- fake GitHub server で受信 header を検証し、想定 credential ID 以外が使われないことを確認する。
- read token で write、write token で policy 外 repository、attacker token、Cookie auth を negative case にする。
- GitHub test organization では repo exact/owner/public、rename/redirect、rate limit、token expiry を確認する。
- 対応対象の pinned `gh` version と更新候補 version で command matrix を実行する。
- `git` protocol v0/v1/v2、submodule、large push、authentication retry を確認する。

## 15. 採用しない案

### `gh` wrapper だけで token を切り替える

command や current directory から対象を推測しやすい反面、`gh api`、`git`、subprocess、extension など全経路を wrapper で漏れなく扱う必要があり、実 credential が sandbox に存在する。防御境界を trusted proxy に集約する目的と合わないため採用しない。環境変数を設定する薄い wrapper 自体は利用する。

### Blind CONNECT proxy

TLS を終端しない proxy は path、GraphQL body、Git service、Authorization を検査・置換できないため、本要件を満たさない。

### Token scope だけに依存する

token が意図より広い場合の defense-in-depth がなく、owner token では owner 内の repository/action を十分細かく制御できない。proxy policy と GitHub token scope の積集合で許可する。

### GraphQL request を token ごとに試行する

resource existence の side channel、rate limit 浪費、identity の不整合を生み、mutation では重複実行の危険があるため採用しない。

## 16. 未決事項

実装前に以下を具体化する。

1. main repository の「write」に含める capability。特に Actions 実行、release、check、workflow file 更新を許可するか。
2. GitHub 上の actor を個人 user、専用 machine user、GitHub App bot のどれにするか。
3. 各 owner へ GitHub App を install できるか。できない owner で PAT approval/rotation を誰が担当するか。
4. 初期対応する `gh` command と pinned version。
5. public GraphQL/search の必要性と public-reader machine user の運用。
6. Git LFS、release assets、Packages、Codespaces の優先順位。
7. 一つの sandbox policy を複数同時 job で共有するか、job ごとに principal を分けるか。
8. proxy の実装言語、secret store、HA/SLO、監査 log の保存先。
9. branch/ref 単位の Git push 制御を proxy で行うか、GitHub ruleset の必須化で十分とするか。

## 17. 参考資料

- [Managing your personal access tokens - GitHub Docs](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens): fine-grained PAT の resource owner、repository access、public repository read
- [GitHub CLI environment variables](https://cli.github.com/manual/gh_help_environment): `GH_TOKEN`、`GH_HOST` など
- [Authenticating to the REST API - GitHub Docs](https://docs.github.com/en/rest/authentication/authenticating-to-the-rest-api): REST API の Bearer authentication
- [Permissions required for fine-grained personal access tokens - GitHub Docs](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens): endpoint ごとの permission と `X-Accepted-GitHub-Permissions`
- [Forming calls with GraphQL - GitHub Docs](https://docs.github.com/en/graphql/guides/forming-calls-with-graphql): `/graphql`、query/mutation、variables、node ID を使う mutation
- [Generating an installation access token for a GitHub App - GitHub Docs](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app): repository/permission を絞った installation token と有効期限
- [Authenticating as a GitHub App installation - GitHub Docs](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation): App installation token による API/Git access
- [GitHub REST API OpenAPI description](https://github.com/github/rest-api-description): REST endpoint catalog の source
- [GitHub REST API versions](https://docs.github.com/en/rest/about-the-rest-api/api-versions): `X-GitHub-Api-Version` と version lifecycle
- [Git HTTP protocol](https://git-scm.com/docs/http-protocol): `info/refs`、`git-upload-pack`、`git-receive-pack`
