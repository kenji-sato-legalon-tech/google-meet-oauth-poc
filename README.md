# Google Meet OAuth PoC

DealOn側で1つのOAuthクライアントを用意し、顧客は「Googleでログイン」するだけで文字起こしを取得できることを実証するPoC。

## 前提条件

- Go 1.21+
- direnv
- Google Workspaceアカウント x2（下記「検証構成」参照）

## 検証構成

PoCでは「DealOn側がOAuthクライアントを持ち、顧客はログインするだけ」という構図を再現する。
そのため、最低2つのWorkspaceアカウントが必要。

| 役割 | アカウント | 用途 |
|------|-----------|------|
| DealOn側（OAuthクライアント管理者） | Workspaceアカウント A | GCPプロジェクト作成、OAuthクライアント作成 |
| 顧客側（エンドユーザー） | Workspaceアカウント B | 「Googleでログイン」して文字起こし取得 |

### 事前準備（アカウントBで実施）

1. Google Meetで会議を開催（文字起こしをONにする）
2. 適当に会話して文字起こしデータを生成
3. meeting code（例: `abc-defg-hij`）をメモ

## GCPセットアップ手順（アカウントAで実施）

### 1. GCPプロジェクト作成（既存プロジェクトがあればスキップ）

1. https://console.cloud.google.com/ にアカウントAでアクセス
2. プロジェクトセレクタ → 「新しいプロジェクト」
3. プロジェクト名: 任意（例: `meet-oauth-poc`）
4. 組織: なしでOK
5. 「作成」

### 2. Google Meet API を有効化

1. GCP Console → 「APIとサービス」→「ライブラリ」
2. 「Google Meet REST API」を検索
3. 「有効にする」をクリック

   または CLI:
   ```bash
   gcloud services enable meet.googleapis.com --project=YOUR_PROJECT_ID
   ```

### 3. OAuth同意画面の設定

1. GCP Console →「APIとサービス」→「OAuth同意画面」
2. User Type: 「外部」を選択 →「作成」
3. アプリ情報:
   - アプリ名: 任意（例: `Meet OAuth PoC`）
   - ユーザーサポートメール: アカウントAのメールアドレス
   - デベロッパーの連絡先: アカウントAのメールアドレス
4. スコープ: スキップ（後で自動的に要求される）
5. テストユーザー:
   - 「ADD USERS」をクリック
   - **アカウントBのメールアドレスを追加**（このアカウントで認証テストする）
6. 「保存して次へ」→ 完了

### 4. OAuthクライアントIDの作成

1. GCP Console →「APIとサービス」→「認証情報」
2. 「認証情報を作成」→「OAuthクライアントID」
3. アプリケーションの種類: 「ウェブアプリケーション」
4. 名前: 任意（例: `Meet PoC Local`）
5. 承認済みのリダイレクトURI:
   ```
   http://localhost:8080/callback
   ```
6. 「作成」をクリック
7. 表示されるクライアントIDとクライアントシークレットをメモ

## ローカル起動手順

```bash
cd poc/google-meet-oauth-poc

# .envファイルを作成
cp .env.example .env

# .envを編集してGCPの認証情報を入力
# GOOGLE_CLIENT_ID=xxxx.apps.googleusercontent.com
# GOOGLE_CLIENT_SECRET=GOCSPX-xxxx

# direnvを許可
direnv allow

# 起動
go run main.go
```

ブラウザで http://localhost:8080 を開く。

## デモ手順（アカウントBで実施）

1. ブラウザで http://localhost:8080 を開く
2. 「Googleでログイン」をクリック
3. **アカウントB** でGoogle認証 → 許可
4. 事前準備でメモしたmeeting codeを入力
5. 「文字起こしを取得」をクリック
6. アカウントBの会議の文字起こしが表示される

### デモのポイント

アカウントBのユーザー（顧客役）がやったことは「Googleでログイン」と「meeting code入力」だけ。
GCPプロジェクト作成やAPI有効化などの手順は一切不要。

## 注意事項

- **テストモード制限**: OAuth同意画面がテストモードの間は、テストユーザーに追加したアカウントのみ認証可能（最大100ユーザー）
- **Workspaceアカウント必須**: Google Meet APIの文字起こし機能はWorkspaceアカウントの会議でのみ利用可能。個人Gmailアカウントの会議では文字起こしデータが存在しない
- **セキュリティ**: PoC用途のため、セッション管理はインメモリ。本番利用は想定していない
- **localhost限定**: リダイレクトURIが `localhost:8080` 固定
