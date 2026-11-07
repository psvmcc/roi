# roi

The name **roi** comes from the Greek **ροή**, meaning “flow” or “stream”.

A single-user RSS/Atom aggregator written in Go, with a Google Reader API for [Reeder Classic](https://reederapp.com/classic/), SQLite storage, and a web admin interface. One process runs three HTTP listeners and the feed scheduler.

## Quick start

Requirements: Go 1.27.3+ and [just](https://github.com/casey/just).

```sh
just run
```

By default, `just run` uses [examples/config.yaml](examples/config.yaml), which contains working local admin test credentials:

| Service | Username | Password |
| --- | --- | --- |
| Admin | `admin` | `roi-admin-test` |

The example includes the admin password as a comment next to its Argon2id hash. Reader API credentials are created in the admin. For your own instance, copy it and generate an admin password:

```sh
just build
cp examples/config.yaml config.yaml
./bin/roi password-hash
# Paste the hash into admin_auth.password_hash.
chmod 600 config.yaml
./bin/roi --config config.yaml --check-config
./bin/roi --config config.yaml
# Or: just run config.yaml
```

`password-hash` reads the password from the terminal without echoing it. Passwords must contain 12–4096 bytes. New hashes use PHC-encoded Argon2id (64 MiB, three passes, four lanes, a random 16-byte salt and a 32-byte key). Legacy bcrypt hashes remain accepted to allow migration; regenerate them with this command. Store the admin hash in YAML. Personal Reader API sign-in codes are generated in the admin and are entered as passwords in your reader.

Default addresses:

| Service | Address |
| --- | --- |
| Reader API base URL | `http://127.0.0.1:8080/greader` |
| Admin | `http://127.0.0.1:8081/` |
| Health check | `http://127.0.0.1:8082/health` |
| Metrics | `http://127.0.0.1:8082/metrics` |

The admin separates subscriptions into **RSS Feeds** and **GIT Releases Feeds**. Each table keeps paused feeds first, followed by feeds with errors, and has its own sync action. Git repositories are added from the Git table. Total unread and storage statistics cover both types. Rename subscriptions with the pencil icon before the name. **RSS settings** can change the RSS URL and override the website URL in place, preserving the subscription ID, folders, articles and read/starred state. HTTP/HTTPS variants of an existing subscription URL are rejected as duplicates. Changing the source clears HTTP cache validators and schedules a fresh sync for enabled, unpaused feeds. Leave the website override empty to use the URL from the feed; manual overrides persist across syncs and ROI export/import. **Icon URL override** is available in both RSS and Git source settings. It overrides the Reader API `iconUrl`, survives syncs and is included in ROI snapshots. Clear the field to restore the automatic icon; the original detected URL remains stored. Settings dialogs have copy buttons beside text fields. Each field has one copy button: it copies the entered override when nonempty, otherwise the known automatic/default value. The button is disabled only when neither value exists. The automatic website URL is retained separately and restored immediately when its override is cleared. RSS/Atom metadata images take priority; otherwise ROI discovers the website’s icon link (including apple-touch-icon) and falls back to `/favicon.ico`. Website discovery is cached for 24 hours, bounded to 256 KiB and two seconds, uses the feed User-Agent and the existing outbound network/host limits. Discovery failures do not fail article sync and retry after an hour. A subscription without an automatic icon gets one unconditional feed request to discover its metadata after upgrade. Responses with HTTP 304 preserve the known metadata icon. Feed actions appear in a single row of icons with tooltips and accessible labels. The **Disabled only** switch beside **Sync all** applies to both tables and shows disabled subscriptions; the default view shows enabled subscriptions. Actions update the dashboard without reloading the page, and statistics refresh every five seconds. The top menu separates **Feeds**, **Starred**, **Devices**, **Security**, **Import/Export** and **Maintenance**, with direct URLs for each page. Menu icons include text on desktop; screens up to 600 px show icons only, with accessible names and tooltips. Devices contains personal API access and Reader sessions; Security contains browser sessions and security events; Import/Export contains OPML and ROI subscription transfers; Maintenance contains storage and backup statistics. Only the open page is queried and refreshed. Menu navigation and browser back/forward work without reloading the document; direct links also work without JavaScript. Recent syncs are collapsed below the feed tables.

The **Starred** counter shows all saved articles, including disabled subscriptions. Open **Starred** to browse their original links, subscription names and publication dates, with 50 articles per page. The list refreshes with the dashboard. Read and starred state is shared across Reader devices; state changes update the existing article without changing its ID or publication date.

Import an OPML file in the admin interface to add subscriptions. URLs are checked across the entire database, regardless of folder. Existing URLs are skipped without changing their titles, folders, pause state, or subscription state. Duplicate URLs within a file are skipped too: the first occurrence wins. Subscriptions missing from an import are retained.

Nested folders become slash-separated names, such as `Dev/Go`. OPML export includes enabled RSS subscriptions, including paused ones, and their folder names. ROI export includes both RSS and Git subscriptions. OPML downloads use UTC timestamps, such as `roi-20261101T120000.000000000Z.opml`. OPML import also treats HTTP and HTTPS variants of the same URL as duplicates, regardless of folder or whether an existing subscription is disabled. Import preserves the existing URL and settings. OPML does not contain articles, passwords, or read/starred state.

The icons in the header select system, light, or dark theme. The choice is saved in the browser and shared between tabs. System mode follows changes to the operating system’s color scheme without reloading.

### Git release and tag sources

Use **Add Git repository** in the admin to subscribe to GitHub, Forgejo, Gitea, GitLab or Gitweb repositories. For example, `https://github.com/nginx/nginx` becomes **github.com :: nginx/nginx** in the reserved **GIT_Releases** folder. Service detection uses known hosts, website metadata and version APIs; select a service explicitly for hosts whose branding or access policy prevents detection. GitLab subgroup paths are supported. GitHub, Forgejo, Gitea and GitLab installations under a URL subpath are not currently supported.

Gitweb URLs use the repository query parameter, for example `https://git.haproxy.org/?p=haproxy-3.4.git;a=summary`. ROI normalizes the URL and names this source **git.haproxy.org :: haproxy-3.4**. Gitweb always tracks tags: it reads the tag table and fetches annotated-tag pages (or commit pages for lightweight tags), preserving source dates and converting messages to safe HTML. Annotated tags also include the linked commit message, which can contain detailed release notes (as in HAProxy). Article links point to the commit; the tag name and tag date are retained. Older tag-only descriptions are refreshed in place on the next sync, preserving article IDs and read/starred state. Stored messages and dates are reused while both the source object identity and HTML renderer version match. Gitweb links and code blocks are preserved through safe conversion. No Git executable or clone is required. Gitweb installations under a URL subpath such as `/gitweb.cgi` are supported; path-info-style repository URLs are not. The complete tag-table response shares the configured download budget with detail pages; each sync retains up to 20 matching tags. Tag messages may be short and do not automatically include CHANGELOG files.

An optional **Tag filter**, editable in Git source settings, applies to all providers and both tracking modes. Use `v3.4.*` to follow only the 3.4 series, or `?` for exactly one character. Matching is case-sensitive; all other characters are literal, except brackets and backslashes, which are rejected. An empty filter accepts all tags. Preview filtering still applies independently. Updating the filter hides nonmatching saved entries from Reader results and unread counts without deleting their history. ROI export/import preserves the filter as `tag_filter`.

Choose **Releases** or **Tags**. The default excludes drafts, prereleases and test/preview version names. **Include test and preview versions** enables prereleases and tags such as `alpha`, `beta`, `rc`, `preview`, `dev`, `nightly` and `snapshot`; drafts remain excluded. Use the branch icon in a subscription's actions to change these options later. For services without an explicit prerelease flag and for tag mode, filtering is a version-name heuristic. Some projects publish only tags; select Tags when their Releases API is empty.

Release notes are converted from Markdown into Reader-compatible HTML, including headings, lists, tables, links and code blocks. Raw HTML and unsafe links are excluded. Relative links become absolute. Descriptions are limited to 100,000 bytes with a link to the full notes. Each source exposes the site's discovered favicon (or `/favicon.ico`) through `iconUrl`. Release dates and available tag/commit dates are retained; undated API records are skipped rather than assigned the sync time. GitHub tag lists omit timestamps, so ROI looks up the referenced commit and uses its committer/author date; subsequent syncs reuse the stored date and contents only if the commit SHA and renderer version are unchanged. Retargeting a tag or upgrading the renderer refreshes the existing article without changing its ID or read/starred state. This can predate the creation of an annotated tag.

Git sources use the normal scheduler, error/backoff counters, retention and SQLite article history. Each sync collects up to 20 matching records from at most five API pages, requesting 20 records per page. Only the latest matching versions are polled: there is no deep-history backfill, saved archive cursor or accelerated catch-up schedule. Already downloaded articles remain in SQLite until retention or an explicit reset removes them. Pagination follows next-page hints when a host clamps its page size. HTTP bodies share `poll.max_bytes` (32 MiB by default), and the entire Git sync is bounded by `poll.timeout`. HTTP 403/429 rate-limit reset headers and Retry-After are respected. Size-limit errors show the request URL, cumulative bytes read (a lower bound when a response is cut short), operation budget and configured `poll.max_bytes`. Filtered preview releases still count toward the downloaded bytes. Turning previews off hides previously stored preview entries from Reader results and unread counts while retaining their history and read/starred state. Switching between releases and tags preserves stable item IDs for the same tag.

Only the admin can create Git sources or restore them from `.roi` files. Reader API clients cannot add/move subscriptions into GIT_Releases or its subfolders, rename/delete that directory, or re-enable a Git source by subscribing to its repository URL. They can read Git entries and unsubscribe (disable) a source; permanent deletion and re-enabling remain available in the admin. ROI exports include provider, mode, preview setting, name, favicon and enabled/paused state; legacy ROI Git OPML files can still be imported. Ordinary RSS entries cannot be imported into the reserved folder.

RSS requests use `User-Agent: roi/<version>` by default (for example `roi/dev` for an unversioned development build). Open **RSS settings** (the sliders icon in a subscription's actions) to override it for that RSS subscription. Clear the field to restore the default. Custom values accept up to 512 printable ASCII characters. Saving clears conditional HTTP cache validators and schedules a refresh; paused and disabled subscriptions remain paused or disabled. Overrides persist in SQLite and ROI exports. Git API requests always use the build User-Agent.

#### ROI subscription format

**Export ROI** downloads `roi-YYYYMMDDTHHMMSS.nnnnnnnnnZ.roi`, a versioned YAML document containing all RSS and Git subscriptions, including disabled and paused ones. The timestamp is UTC. **Import ROI** validates the complete file before committing it atomically. Existing URLs are skipped across all folders and source types, treating HTTP and HTTPS variants of the same URL as duplicates; the first occurrence of a duplicate in a file wins. Existing subscriptions are never renamed, reconfigured or re-enabled by import. RSS folders and website URLs are preserved. Git sources always belong to GIT_Releases. Import does not perform network requests; the normal poller fetches enabled sources afterward.

```yaml
format: roi
version: 1
sources:
  - type: rss
    url: https://go.dev/blog/feed.atom
    site_url: https://go.dev/blog/
    title: Go Blog
    folders: [Development/Go]
    enabled: true
    paused: false
  - type: git
    repository: https://github.com/nginx/nginx
    service: github
    title: 'github.com :: nginx/nginx'
    mode: releases
    include_preview: false
    enabled: true
    paused: false
```

See [examples/subscriptions.roi](examples/subscriptions.roi). `type` is required (`rss` or `git`). RSS sources require `url`; `site_url`, `folders` and `user_agent` are optional. RSS sources cannot use GIT_Releases or its subfolders. Git sources require `repository` and a detected `service` (`github`, `forgejo`, `gitea`, `gitlab`); `mode` defaults to `releases`. For both types, `enabled` defaults to true and `paused` to false. Git preview inclusion defaults to false. `title` defaults to the RSS URL or host/repository name; `icon_url` is optional. Unknown fields, unsupported versions, missing/unknown types, mixed RSS/Git fields, multiple YAML documents and files larger than 8 MiB or 10,000 sources are rejected. RSS folder names must contain 1–512 characters without control characters. Source settings are included; API tokens, passwords, sessions, articles and read/starred state are excluded. Old `.roig` / `roi-git` files are not supported. Regular OPML export contains only enabled RSS feeds.

Public repositories work without an API token, subject to host rate limits. To increase the allowance or access permitted private repositories, configure a token per repository host:

```yaml
git:
  tokens:
    github.com: github_pat_replace_me
    # gitlab.example.com: replace_me
```

Tokens are sent only to that host's API (or api.github.com for github.com), never to the website/favicon discovery request or across an origin-changing redirect. The same private-network/DNS restrictions as RSS apply; internal Git hosts require `poll.allow_private_networks: true`. No extra service, worker or CGO dependency is required. Markdown rendering adds one pure-Go dependency, goldmark.


## CLI and environment variables

The CLI uses [urfave/cli/v3](https://cli.urfave.org/v3/examples/flags/value-sources/). Values take precedence in this order: command-line flag, environment variable, default.

```sh
ROI_CONFIG=/etc/roi/config.yaml ./bin/roi
ROI_CONFIG=examples/config.yaml ROI_CHECK_CONFIG=true ./bin/roi
./bin/roi --config examples/config.yaml --check-config
./bin/roi -c /etc/roi/config.yaml
./bin/roi --help
./bin/roi password-hash --help
```

Set `api.public_url` to the external API origin (for example `https://rss.example.org`, optionally including a port). ROI appends `api.prefix` automatically: with `/greader`, the credentials dialog shows `https://rss.example.org/greader` alongside the login and one-time code, with a copy button. The same URL appears when regenerating a code and in the HTML response without JavaScript. Leave it empty to omit the URL. It is a display setting and does not change listeners; paths, credentials, query strings and fragments are rejected.

Create a personal Reader API access in **Reader API devices → Add API device**. Enter a device name; roi generates a unique, permanent login and a random one-time sign-in code. The plaintext code is shown only in that response and cleared from the credentials dialog when it closes. Copy both values into a Google Reader API account in Reeder, using the code as its password. The code expires after 24 hours or its first successful `ClientLogin`; it is stored only as an Argon2id hash and is erased on consumption. Consuming the code and creating its session form one SQLite transaction. Wrong passwords do not consume it, and concurrent attempts can create at most one session. A lost login response requires generating a new code.

Every successful sign-in creates a named **Pending approval** session. The client can finish account setup but receives empty subscription, folder, unread, item ID and article responses until an admin approves it; mutations are denied. Select **Approve device** in **Reader API sessions**, confirm the name, then sync the client again. Every new session requires approval, including a second sign-in for the same device. Sessions do not expire automatically. Use a different personal login on every physical device; clients that synchronize one shared account/token still share that session.

**Generate new sign-in code** replaces any unused code after confirmation, preserving the login and existing sessions. Use it if a code was lost/expired or Reeder needs to authenticate again. Renaming a device updates its session names without changing the login. **Delete API device** requires confirmation and revokes all of that device's sessions; subscriptions and reading state are preserved. Individual session revocation and **Hide feeds / Show feeds** remain available.

`./bin/roi --config config.yaml rotate-api-key` rotates the persisted signing key while roi is stopped. For immediate revocation on a running instance, use **Revoke API tokens** in the admin. Both actions revoke Reader sessions and invalidate unused sign-in codes; admin sessions stay signed in. Generate new codes to reconnect.

The deprecated `api_auth` YAML block never accepts new logins. During an upgrade, keep the existing block for the first startup so roi can preserve validation of existing legacy tokens in SQLite; it can then be removed. Existing sessions are not automatically migrated to personal logins. Add a personal device for each client, reconnect and approve it, verify separate session IDs, then manually revoke the old shared sessions. Changing a retained legacy `api_auth` block revokes only legacy sessions; personal device sessions are independent of that config. The public example has no `api_auth` block.

The **Reader API sessions** block lists active Reader sessions with individual revocation and a revoke-all action. **Browser sessions** appears immediately below it for active admin sessions. Revoked and expired sessions are hidden from both lists. Each table shows 100 sessions per page, with Previous/Next navigation and the total active count. Approved Reader sessions remain accessible on later pages even when pending requests fill the first page. Tables include creation, last use, first/last IP, request count and client. Browser sessions additionally show expiry. Each session has an individual **Revoke session** action. Revoking the current browser session signs it out. Both API and browser sessions survive restarts. Reader API sessions remain valid until explicitly revoked, device deletion or signing-key rotation. The legacy `security.token_ttl` configuration field is accepted but ignored. Browser sessions expire 24 hours after login; changing the admin username or password hash revokes all browser sessions on startup, independently of API sessions. IP and User-Agent changes do not end a session. SQLite stores the hashed cookie identifier and CSRF token; the raw cookie stays in the browser. At most 100 unapproved Reader sessions remain active; new logins revoke the least recently used pending session without evicting approved clients. Security-event snapshots remain available. At most 100 browser sessions remain active; a new login revokes the least recently used session when necessary. Upgrading from a version without persistent browser sessions requires signing in once. Upgrading from a version without registered API sessions requires Reader clients to authenticate once again. Session identifiers displayed in the admin are SHA-256 digests, never bearer tokens. The **Security events** table shows the latest 100 failed logins, rejected requests, rate limits, successful logins, device approvals and session revocations. Security metadata persists in SQLite for up to 30 days and 10,000 events. Ordinary requests are written to server logs only when `debug: true`. Static assets and background fragment refreshes are omitted from the event log. With `debug: true`, API requests and admin actions include the client IP and device identity in server logs. Forwarded addresses are accepted only from explicitly trusted proxies.

The binary defaults to `config.yaml` in its working directory. `ROI_CONFIG` overrides the configuration path; `ROI_CHECK_CONFIG` validates it without starting the servers or opening the database. Other application settings are configured in YAML.

## Reeder Classic

Choose a **Google Reader API** account in [Reeder Classic](https://reederapp.com/classic/). Enter the Reader API base URL, the personal login created in the admin, and its unused sign-in code as the password. For example, use `https://rss.example.org/greader`. Do not append `/reader/api/0` or `.php`.

Implemented endpoints: `accounts/ClientLogin`, `token`, `user-info`, `preference/list`, `preference/stream/list`, `subscription/list`, `tag/list`, `unread-count`, `stream/items/ids`, `stream/items/contents`, `stream/contents/*`, `edit-tag`, `mark-all-as-read`, `subscription/quickadd`, `subscription/edit`, `rename-tag`, and `disable-tag`.

On first start after upgrading from schema 1, roi migrates the database to schema 2 and creates an independent random signing key. Previously issued Reader tokens are invalidated; clients must authenticate again. Article history, IDs, subscriptions, and read/starred state are preserved.

Every authenticated request requires `Authorization: GoogleLogin auth=...`. Mutations require POST. The action token endpoint returns a 57-character token; Reeder’s `T=x` and omitted action tokens are also accepted after bearer authentication. Admin authentication uses a separate cookie and always requires a CSRF token for mutations. Reader preferences currently return empty defaults. Arbitrary article labels are not supported.

Item date fields `published` (seconds) and `timestampUsec` (microseconds) use the source publication date so Reeder can show original post dates. `crawlTimeMsec` remains the immutable time roi first received the item. RSS `pubDate` / Atom `published` takes priority, then the source `updated` date; when neither exists, the first receipt time is used and retained across later polls. A source date-only correction updates the article metadata while preserving its ID, receipt time, and read/starred state. Feed publication statistics are derived from stored article dates, so corrected or removed articles cannot leave a stale cached maximum in the admin.

Previously cached articles in Reeder may retain their old displayed dates until the client re-downloads them. Refresh first; if necessary, perform a full client resync or re-add the account. The server history and read/starred state do not need to be deleted.

Item references use decimal ID strings; article content uses the corresponding hexadecimal Google Reader item IDs. Incoming IDs may use decimal, bare hexadecimal (including Reeder’s zero-padded format), `0x` hexadecimal, or the long Google Reader form. Paginated results omit `continuation` on the last page. Cache-busting parameters such as `ck` can change between pages. Both repeated parameters (`i=...&i=...`) and array notation (`i[]=...`) are accepted.

The API has integration tests for login, paging, contents, state changes, and subscriptions. Actual compatibility must also be verified with Reeder Classic: initial sync, subsequent sync, offline read/starred changes, mark-all-as-read, folder/subscription edits, and server restarts.

### Troubleshooting sync

Check the admin interface first: subscriptions can be imported before their articles have been fetched. Review feed errors and the entry count. Unavailable feeds do not prevent other feeds from syncing.

Admin/API request logs are disabled by default. Set top-level `debug: true` in YAML to enable DEBUG logs with device name, approval state, session ID, IP, client, method, path, status, response size and duration. Unapproved API clients are labeled `unapproved` and colored red when stderr is a terminal; redirected logs remain plain text. Request passwords, bearer tokens, query strings and bodies are omitted. Feed-fetch warnings always include feed ID, title and the complete source URL; feed errors retain complete URLs in both logs and the admin. Security events remain stored and visible when debug is disabled.

After upgrading, restart roi and refresh Reeder. If a failed initial sync has left a client account empty, inspect the new request log before resetting or re-adding the account.

## Configuration

Configuration uses YAML with `#` comments. Unknown fields, duplicate keys, multiple documents, and invalid values are rejected. Restart roi to apply changes.

- `api.listen`, `admin.listen`, `ops.listen`: separate `host:port` addresses.
- `api.prefix`: base path without a trailing slash; an empty string is allowed. Internal protocol paths are fixed.
- `api.public_url`: optional external HTTP(S) origin shown with sign-in credentials; the API prefix is appended automatically.
- `database`: SQLite file path, relative to the process working directory if not absolute.
- `admin_auth`: admin username and password hash. Reader API devices and one-time codes are managed in the admin and stored in SQLite. `api_auth` is accepted only to migrate existing legacy sessions.
- `poll.interval`: base interval, such as `15m` or `1h`; allowed range: 1 second to 720 hours.
- `poll.timeout`: HTTP request timeout, including redirects and response reading.
- `poll.workers`: 1–32 concurrent workers across different host groups.
- `poll.per_host_concurrency`: maximum complete syncs per host group (default 1, range 1–32).
- `poll.per_host_delay`: minimum pause after a complete sync before another subscription on that host starts (default `2s`, range `0s`–`1h`).
- `poll.max_bytes`: maximum decompressed response size for RSS, or total decompressed API response budget per Git sync; default: 32 MiB (33554432 bytes).
- `poll.allow_private_networks`: allow local/private network sources. Disabled by default; DNS results and redirect destinations are checked.
- `retention.months`: calendar months since first receipt; zero disables the age limit.
- `retention.entries_per_feed`: maximum entries per feed; zero disables the count limit.
- `retention.keep_starred`: protect starred entries from deletion.
- `secure_cookies`: enable when accessing the admin through HTTPS; leave disabled for local HTTP.

For remote access, put the API and admin behind an HTTPS reverse proxy that preserves the API prefix. TLS terminates at the proxy. Keep the unauthenticated operations listener inside your monitoring network. Additional proxy authentication in front of the Reader API interferes with client authentication. By default, roi ignores `X-Forwarded-For` and limits the immediate peer. Explicit `security.trusted_proxies` configuration enables safe forwarded-client addressing as described below.

## Scheduling and history

No external cron is needed. The next fetch time is stored in SQLite. After a restart, workers gradually process overdue feeds without replaying missed intervals. Scheduling adds ±5% jitter, increases delays on errors up to one day, and honors `Retry-After` up to one day. Fetching supports ETag, Last-Modified, HTTP 304, redirect limits, and response size limits.

GUID/Atom ID is unique within a feed. Missing IDs fall back to the article URL, then a hash of the title and publication date. A source that changes an item’s ID may create a new entry. Removing an article from the current feed does not delete it from roi. Content updates preserve read/starred state. roi stores the latest content version from RSS/Atom; it does not download full web pages or attachments.

Subscriptions have three visible states:

| State | Automatic fetching | Reader API | Admin actions |
| --- | --- | --- | --- |
| Enabled | Yes | Visible | Sync, pause, disable |
| Paused | No | Visible | Resume, disable |
| Disabled | No | Hidden, including articles and counters | Enable, delete permanently |

**Disable** preserves articles, read/starred state, folders, and sync history, cancels an in-flight fetch, and hides the subscription from OPML export. **Enable** restores visibility and resumes polling. Reader API `unsubscribe` also disables the subscription; it never permanently deletes data. Subscribing explicitly to the same URL through the API re-enables it. OPML imports skip known URLs, including disabled ones.

Permanent deletion is available only in the admin, only for disabled subscriptions, and requires a confirmation page and CSRF-protected POST. It atomically removes articles, read/starred state, sync history, deleted-item identifiers, and folder links. IDs are not reused. Configured retention still applies to stored articles, including disabled subscriptions.

Times are stored in UTC. The admin displays date and time as `YYYY.MM.DD HH:mm:ss` in the browser timezone without a GMT/UTC suffix, including refreshed fragments; without JavaScript, UTC remains available as a fallback. Backup schedules and retention boundaries still use UTC. The admin shows the last attempt and success, receipt of new entries, latest source publication, next fetch, errors, a consecutive error counter per feed, and the latest 50 fetch attempts. The counter increases after each failed sync, persists across restarts, and resets to zero after a successful HTTP 200/304 sync. Cancellation caused by disable or shutdown does not count as a sync failure. Fetch logs are kept for 30 days, independently of article retention. Article HTML is not rendered in the admin interface.

## Retention and storage size

By default, `months: 0` and `entries_per_feed: 0` retain articles indefinitely. Example:

```yaml
retention:
  months: 12
  entries_per_feed: 10000
  keep_starred: true
```

Age is measured from first receipt, not publication. Months are calendar months in UTC. When both limits are enabled, exceeding either makes an entry eligible for deletion. Protected starred entries can exceed the count limit; unread entries have no separate protection. Deletion is permanent, so back up the database before enabling retention.

Retention runs every minute, deleting up to 1,000 entries per transaction. Count limits use receipt time and stable ID order. Compact identifiers of deleted entries are retained indefinitely to prevent old articles from reappearing. Retention therefore bounds article contents, not the total SQLite file size.

Per-feed and total entry sizes count stored UTF-8 text fields. They do not allocate SQLite indexes or free pages to individual feeds. The complete database and WAL sizes are shown separately. Deletion frees pages for reuse and usually does not shrink the file. To reclaim disk space, stop roi and run `sqlite3 data/roi.db VACUUM`; this needs additional free disk space.

## SQLite, backups, and operations

The driver is [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite). CGO and a system SQLite library are not required to build or run roi. `just build` sets `CGO_ENABLED=0`. The Go race detector itself requires CGO and runs separately from the tests without CGO.

Use one roi process per database on a local filesystem. SQLite uses WAL, foreign keys, and a lock timeout. Automatic checkpoints run at 1,000 WAL pages; reusable journal space is limited to 4 MiB after a successful reset. After the sync queue drains, roi attempts a nonblocking TRUNCATE checkpoint to copy committed changes into the database and release WAL space. Maintenance repeats every minute (PASSIVE while feeds are syncing, TRUNCATE while idle), including changes made through the API. External readers can delay checkpoint completion, so the WAL size is not a hard limit; roi retries during later maintenance. WAL files must never be deleted manually while the database is open. Versioned schema changes are transactional; unsupported newer schema versions are rejected. SIGINT/SIGTERM stop fetching and shut down the HTTP servers gracefully.

Daily online backups are built in and require no cron:

```yaml
backup:
  enabled: true
  directory: /backups/roi
  time: "03:00" # UTC
  keep: 7
```

The scheduler creates `roi-YYYYMMDD.sqlite` once per UTC day using SQLite's online backup API, including committed WAL data, through a separate connection. The application stays available during copying. The completed backup is integrity-checked, synced and published atomically with file mode 0600. Existing daily copies are verified and never overwritten. After a successful backup, only the newest `keep` daily copies remain; unrelated files and symlinks are ignored. The default is disabled, time 03:00 UTC, directory `backups/automatic` next to the database and 7 copies. A restart after the configured time catches up for the current day; missed days are not replayed. Failures are logged and retried after 15 minutes; incomplete files are removed and existing backups are retained. Maintenance shows the schedule, next run (including retries), current copy count and disk usage, recorded lifetime creation totals, errors, stored filenames and paginated UTC daily history. Task runs include startup checks of existing daily files and retries; copies created count only newly published files. Daily accounting persists in SQLite and survives automatic file retention and restarts. It starts with this version; existing files are included in current storage totals, but deleted historical copies cannot be reconstructed. Created bytes are cumulative output, not current disk usage. The backup directory should be dedicated to this instance. In containers, mount a writable persistent backup directory.

Metadata and dashboard statistics use transactionally maintained per-feed counters instead of scanning article bodies. Folder membership is read in one query. The admin Unread total and enabled-feed unread counts match Reader visibility, including preview and tag filters. The summary separately shows stored unread articles (including hidden and disabled history). Existing statistics are backfilled once during the schema upgrade.

Retention records retired GUIDs as fixed 32-byte SHA-256 fingerprints. Legacy text GUIDs are compacted in bounded maintenance batches. Fingerprints are never expired: a hard bound on their count would lose exact suppression and could resurrect old articles. They are much smaller than retained article bodies but still grow with the number of distinct retired articles. `roi_tombstones` and `roi_tombstone_key_bytes` expose this overhead; `roi_database_free_bytes` reports reusable SQLite space. `roi_unread_entries` counts all stored unread entries, while `roi_reader_unread_entries` counts only entries visible to clients. The admin footer also shows reusable space and retired-key counts.

Deleting articles makes database pages reusable; it does not necessarily shrink the file. To reclaim disk space, stop roi and run:

```sh
roi --config config.yaml compact
# Or: just compact config.yaml
```

The command takes the same exclusive instance lock as the server, checks available disk space for a backup and VACUUM, creates and verifies an independent timestamped `*.before-compact-*.sqlite` backup, runs VACUUM, truncates the idle WAL and checks integrity. Pre-compaction backups are retained for manual review and are not included in automatic daily retention. It refuses to run against an active roi instance. Normal syncing never runs a blocking full VACUUM. Restore tests start the application from a snapshot and verify articles, read/starred state, persistent browser/Reader sessions and derived counters.

Metrics: `roi_backups_total`, `roi_backup_errors_total`, `roi_backup_duration_seconds` and `roi_backup_last_success_timestamp_seconds`. Backups contain articles, read/starred state and sessions; the YAML config and Git API tokens must be saved separately. To restore, stop roi and other SQLite clients, then run `roi --config config.yaml restore --from /backups/roi/roi-YYYYMMDD.sqlite`. The command stages and checks the backup, migrates it, rotates the API signing key, revokes all restored browser and Reader sessions and clears restored HTTP cache data before publishing it. Previous database/WAL/SHM files are preserved in a private `roi-before-restore-*` directory. All clients must authenticate again; Reader devices require approval. An invalid backup leaves the existing database unchanged. Manually copying an old database can revive sessions that were revoked after the backup; use the restore command instead.

Subscription-only backups can run independently of SQLite backups:

```yaml
subscription_backup:
  enabled: false
  directory: ./data/backups/subscriptions
  time: "03:00" # UTC
  keep: 30
```

When enabled, the first `.roi` snapshot is created immediately if no copies exist. Subsequent checks run once daily at the configured UTC time. A new `roi-YYYYMMDD.roi` file is written only when the deterministic subscription export differs from the latest copy. Changes made after today's copy or check wait until the next day. Check state persists in SQLite, so restarts do not create extra copies; a restart after the scheduled time catches up on a missed check. Failed checks retry after 15 minutes. Copies are published atomically with mode 0600 and never overwrite existing files. The default retention is 30 copies, not 30 days; unchanged exports may skip any number of days. Use a dedicated directory per instance. Only canonical automatic `.roi` filenames are rotated; unrelated files, symlinks and links to the live database are ignored. Maintenance shows the schedule, last check and result, last creation, errors, current storage and recorded creation totals. Import a copy through **Import/Export** to restore subscriptions. ROI snapshots contain subscription settings only, without articles, read/starred state, sessions or credentials. Back up SQLite separately when those are needed.

For a simple backup, stop roi and copy its configuration and database. For a running database, use SQLite’s backup API, for example `sqlite3 data/roi.db ".backup 'backup.db'"`. Copying only the `.db` file while roi is running may lose changes stored in WAL. Verify restoration separately.

`/health` returns 200 when SQLite is available. Individual source failures do not make roi unhealthy. `/metrics` uses `github.com/VictoriaMetrics/metrics` to expose Prometheus-format fetch counts, errors, entry counts, fetch durations, retention deletions, database sizes, and process metrics. Metrics do not include secrets.

## Development and containers

The project layout separates application code, embedded web resources and editable examples:

```text
cmd/roi/                 CLI and process startup
internal/roi/            Application, Reader API, scheduling and persistence
  admin_*.go             Admin actions, authentication, dashboard and rendering
  store_*.go             SQLite migrations, feeds, authentication and retention
  git_*.go               Git source detection, HTTP requests, entries and Markdown
  opml.go                OPML import/export
  subscriptions.go       ROI import/export
internal/web/
  embed.go               Bundles templates and assets into the executable
  templates/             Admin HTML templates
  assets/                CSS, JavaScript and favicon
examples/
  config.yaml            Local example configuration used by just run
  subscriptions.roi      Example RSS and Git subscriptions
scripts/                 Artifact tooling
tests/                   Browser integration and artifact tooling tests
```

Go tests stay beside their implementation; shared application test helpers live in `helpers_test.go`. Web resources are embedded with `go:embed`, so deployments still need only the executable and runtime configuration. Relative database paths are resolved from the process working directory; moving the example config into `examples/` does not change the default database location.

```sh
just fmt
just check
just build
```

Checks include gofmt, go vet, staticcheck, the race detector, tests without CGO, and govulncheck. The race-enabled suite writes `.coverage/coverage.out` and requires at least 90% total Go statement coverage. Tool versions are pinned in the justfile and CI; module versions and checksums are in `go.mod` and `go.sum`. GitHub Actions tests and builds Linux amd64 and smoke-tests the container. `just lint-actions` validates all workflows with actionlint; CI runs it too. Go 1.27.3 is consistent across `go.mod`, CI, and Dockerfile.

The final container uses `scratch`: a stripped static binary, root CA certificates, a data directory, and an empty temporary directory. It runs as UID/GID 10001 with no shell or C runtime.

```sh
docker build -t roi:local .
docker image inspect roi:local --format '{{.Size}}'
```

For a container configuration, set listeners to `0.0.0.0:8080`, `0.0.0.0:8081`, and `0.0.0.0:8082`, and set the database path to `/data/roi.db`. Mount the YAML file read-only and make the data directory writable by UID 10001:

```sh
docker run --rm --name roi \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 -p 127.0.0.1:8082:8082 \
  -v "$PWD/data:/data" -v "$PWD/config.yaml:/config/config.yaml:ro" roi:local
```


### Podman Quadlet

[examples/roi.container](examples/roi.container) follows the macdack deployment layout and tracks `ghcr.io/psvmcc/roi:stable`. Install it on a Linux amd64 host with Podman, Quadlet and cgroup v2:

```sh
sudo install -d -m 0750 -g 10001 /etc/roi
sudo install -d -m 0700 /var/lib/roi
sudo install -d -m 0755 /etc/containers/systemd
sudo install -m 0600 config.yaml /etc/roi/config.yaml
sudo chown 10001:10001 /etc/roi/config.yaml
sudo install -m 0644 examples/roi.container /etc/containers/systemd/roi.container
sudo systemctl daemon-reload
sudo systemctl start roi.service
sudo systemctl enable --now podman-auto-update.timer
```

Prepare a private configuration first: use listeners `0.0.0.0:8080`, `0.0.0.0:8081`, `0.0.0.0:8082` and database `/data/roi.db`, replace the test passwords, and keep backups under `/data/backups/automatic` (the default for this database path). Both the database and backups then persist in `/var/lib/roi`. The `U` volume option changes that dedicated directory's ownership to UID/GID 10001; `Z` applies a private SELinux label. The configuration directory is mounted read-only so atomic replacements remain visible; replacement files must also be readable by UID 10001. Configuration changes require `systemctl restart roi.service`.

The Quadlet does not publish host ports; configure networking for your deployment. The container has a read-only root filesystem, no capabilities, a 300 MiB temporary filesystem, 128-process limit and 512 MiB memory limit. There is no shell inside the image. Increase memory and temporary space together if you raise parser/response concurrency. Forwarded addresses are trusted only when the actual proxy peer is explicitly configured; Podman bridge/NAT may change that peer, so verify it before setting `trusted_proxies`.

The `[Install]` section supplies the boot dependency; do not run `systemctl enable roi.service` on the generated unit. `AutoUpdate=registry` checks changed stable images through the Podman timer, while `Pull=newer` checks on service start. Configure GHCR authentication for the root account if the package is private. To pin a version instead, change `Image=` to its release tag and remove `AutoUpdate=`.

### CI and releases

Each event runs the single `roi` workflow in `.github/workflows/ci.yml`, with sequential, separately rerunnable jobs: **Linters → Tests → Build and smoke-test → Publish image → Publish GitHub Release**. A push to `main` publishes `ghcr.io/psvmcc/roi:latest`. Pull requests run only the first three jobs with read-only repository permissions; branch pushes do not duplicate these PR checks. Version tags such as `v1.2.3` or `1.2.3` must be reachable from the fetched `main` and publish their exact image tag, updating `stable` for a stable release. The GitHub Release job runs only for version tags. No arm64 artifacts or images are built. Tests upload available coverage and browser diagnostics; the container smoke test validates ENV configuration, `/health` and `/metrics` as a non-root user with a read-only root filesystem.

The build job uploads the smoke-tested image as a Docker archive with a SHA-256 checksum, plus a separate executable/release bundle. Publication downloads the exact build artifact by ID, verifies its checksum and source revision, loads that image and pushes it without rebuilding. Artifacts are retained for 30 days. In a failed run, select **Re-run failed jobs**, or select **Re-run job** on the failed stage. Earlier successful jobs do not need to run again: a GHCR failure can retry only image publication, and a GitHub Release failure can retry only that release stage. Rerunning a build replaces its saved artifacts for subsequent dependent jobs. Expired/deleted artifacts require rerunning the build and its dependent jobs. Registry write permission is limited to image publication, and repository write permission to GitHub Release publication. A new release stays a draft until its files are uploaded.

Dependabot's scheduled dependency scans are separate GitHub-managed runs, not additional build jobs. Go modules, Docker and GitHub Actions are checked weekly; GitHub Actions updates are grouped into one PR. An initial scan or a Dependabot configuration change can trigger these maintenance runs independently of the application workflow.

The release also publishes a GitHub Release with the static `roi-linux-amd64` executable, SHA-256 checksum, CycloneDX inventory, example configuration and Quadlet. Tags such as `v1.2.3-rc.1` produce prereleases and do not update `stable` or GitHub's latest release. Build metadata (`+...`) is rejected because registry tags cannot contain `+`. The `latest` container follows `main`; use `stable` or a specific release in production. Release tags should be pushed after merging to `main`. Restrict their creation through repository rulesets.

Publishing uses the repository's built-in `GITHUB_TOKEN`; package write access is scoped to publishing jobs. Enable Actions and allow that token to publish the repository's GHCR package. Creating these workflow files does not push a tag, publish an image or create a remote release locally.

### Test coverage and browser tests

```sh
just coverage          # .coverage/index.html and .coverage/coverage.out
just fuzz              # bounded ID-parser fuzzing, 15 seconds
just fuzz 1m
```

Go tests cover subscription lifecycle, all Reader API routes, state/filter/paging semantics, authenticated and unauthenticated errors, admin forms and CSRF, OPML rollback and deduplication, retention and tombstones, fetch cancellation and failure recovery, SSRF address policies, YAML validation, database persistence, CLI/ENV, and password hashing. Coverage measures executed statements, not proof of correctness; unreachable OS failures and actual terminal prompting retain some uncovered paths.

Real browser tests use Playwright only as a development dependency, installed outside the source tree:

```sh
npm install --prefix /tmp/roi-browser --no-package-lock --no-audit --no-fund playwright@1.64.1
/tmp/roi-browser/node_modules/.bin/playwright install chromium
PLAYWRIGHT_MODULE=/tmp/roi-browser/node_modules/playwright just browser
```

The browser suite creates temporary databases and localhost-only RSS servers. It checks login/logout, theme persistence and system changes, OPML import deduplication, actual article fetching, Reeder ID encoding, disabling and re-enabling, API unsubscribe, permanent admin deletion, and mobile layout. It never uses your configured database or existing subscriptions. CI runs this suite separately and uploads the Go coverage profile.

### Authentication, proxy trust, and request limits

API token signatures use a random 256-bit key persisted in SQLite, independently of the password hashes in YAML. Knowing the API hash alone cannot produce a valid token. Reader API sessions do not expire automatically. Existing active API sessions are upgraded on startup without requiring a new client login. Revoking tokens invalidates sessions and unused sign-in codes. Changing the deprecated API config affects legacy sessions only. Protect database/WAL backups as well as the configuration.

The `security` section supports:

| Setting | Default | Behavior |
| --- | --- | --- |
| `trusted_proxies` | `[]` | IP addresses / CIDRs allowed to supply `X-Forwarded-For` or `X-Real-IP`; `/0` trust is rejected |
| `max_response_bytes` | `67108864` | Maximum encoded article response, 1 KiB to 256 MiB |
| `login_per_minute` | `10` | Login attempts per client IP and authentication realm |
| `account_login_per_minute` | `60` | Login attempts across all IPs, separately for admin and Reader |
| `api_requests_per_minute` | `6000` | API token-bucket refill rate per IP and authenticated account |
| `api_burst` | `200` | Initial/maximum API bucket capacity |
| `api_concurrency` | `4` | Concurrent authenticated API requests; excess requests queue for up to 5 seconds |

Login budgets persist in SQLite and survive restarts. A shared per-account gate prevents source-IP rotation from creating unlimited login attempts or database rows. API request buckets are bounded in memory and return HTTP 429 with `Retry-After` when exhausted. They do not trust client-supplied addressing by default.

If roi is behind a proxy, list its actual addresses or networks. Individual IPv4/IPv6 addresses and CIDRs can be mixed in one YAML list:

```yaml
security:
  trusted_proxies:
    - 127.0.0.1
    - '::1'
    # Add only the actual network used by your upstream proxies:
    # - 10.20.0.0/24
```

An empty list ignores forwarding headers. roi uses `X-Forwarded-For` only when the immediate socket peer is trusted, walks the chain from right to left and stops at the first untrusted hop. Repeated XFF header lines are combined in order. `X-Real-IP` is accepted from a trusted immediate peer only when XFF is absent. XFF takes precedence if both headers are present. An invalid/empty XFF chain does not fall back to X-Real-IP. Invalid addresses encountered while walking the trusted part of the chain, or an overlong chain (over 20 hops or 4 KiB), fall back to the socket peer. Untrusted left prefixes are ignored after the first untrusted hop. Ambiguous repeated X-Real-IP headers are ignored. Forwarded values must be IP addresses without ports or scope IDs. IPv4-mapped IPv6 addresses are normalized. The standardized `Forwarded` header and the misspelled `X-Forwarder-For` header are ignored.

These rules apply consistently to session IPs, security events, server logs and per-IP rate limits. Keep proxy CIDRs narrower than client networks; default-route networks (`0.0.0.0/0`, `::/0`) are rejected.

For nginx on the same host, add these directives inside the existing TLS `server` block and trust `127.0.0.1` / `::1` in roi:

```nginx
proxy_set_header Host $http_host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

location /greader/ {
    proxy_pass http://127.0.0.1:8080;
}
location / {
    proxy_pass http://127.0.0.1:8081;
}
```

The API prefix is preserved because `proxy_pass` has no URI suffix. Set `secure_cookies: true` for HTTPS access. If nginx itself receives traffic through another proxy, configure its trusted peers and real-IP handling explicitly; do not copy a client-supplied header directly into X-Real-IP. See the [nginx proxy module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header) and [real-IP module](https://nginx.org/en/docs/http/ngx_http_realip_module.html).


Article responses are encoded one entry at a time into a private, byte-limited temporary spool. SQLite is released before network transmission. An oversized batch returns HTTP 413 before any article JSON is sent; no requested batch is silently truncated. Request fewer item IDs or a smaller `n` and retry. Memory scales with the largest article and encoder buffers rather than the full batch. The spool is immediately unlinked on Linux/macOS and removed when closed; the Windows fallback removes it after closing. The temporary filesystem must have room for the response budget multiplied by API concurrency. Defaults bound active spool contents to 256 MiB. A 300 MiB tmpfs allows additional filesystem overhead.

### Pinned builds and inventory

GitHub Actions use immutable commit SHAs. The Go builder image uses a multiarchitecture manifest digest. Dependabot proposes reviewed updates for Go modules, actions, and Docker pins; it does not automatically merge or deploy them.

`just artifacts` produces `bin/roi.cdx.json` (CycloneDX 1.6 inventory of the actual binary's Go runtime and linked modules) and `bin/roi.sha256`. This uses Python's standard library and Go build metadata; it does not add runtime dependencies. Module `h1` sums are recorded as Go properties rather than mislabeling them as file SHA-256 digests. Local module replacements are rejected when packaging. CI builds Linux amd64 binaries with SBOM/checksum sidecars as downloadable artifacts. Version tags publish a GitHub Release and tested GHCR image as described above.

For a restrictive deployment, use read-only root storage, a writable quota-controlled data volume, a bounded tmpfs, no Linux capabilities, and resource limits. For example, after configuring the three listeners for `0.0.0.0`:

```sh
docker run --rm --name roi --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges --pids-limit=128 --memory=512m \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=300m \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 -p 127.0.0.1:8082:8082 \
  -v "$PWD/data:/data" -v "$PWD/config.yaml:/config/config.yaml:ro" roi:local
```

Use HTTPS and `secure_cookies: true` for remote admin access, keep operations ports private, and replace the public test passwords before exposing a service. Restrict egress at the network level when allowing internal feeds. Retention remains opt-in and keeps tombstones; use disk quotas/monitoring if you need a strict database storage bound. These deployment choices are documented here rather than kept in a separate review file.

The favicon combines the Greek rho (ρ) with RSS waves in the admin color palette. It is an embedded SVG and requires no external assets. Permanent subscription deletion opens a confirmation dialog with Cancel focused by default.

All session/token revocations open a confirmation dialog, including individual API/browser sessions, all Reader tokens and all browser sessions. Revoking all browser sessions also signs out the current browser. Security events store the device name and target session ID at the time of the action.

Paused feeds appear first with a yellow stripe, followed by feeds with sync errors and then healthy feeds. The pencil before each feed/device name follows the text size. Approved Reader clients can subscribe through `subscription/quickadd` or `subscription/edit` using their bearer token even when Reeder sends a client-specific `T` value. Pending clients still receive HTTP 403 for every mutation.

### Shared RSS and Git host queues

RSS and Git sources use the same host queue. Hostnames are normalized without port numbers; `github.com` and `api.github.com` form one group. By default one complete subscription sync runs per group, including all Git API pages and tag-date lookups, followed by a two-second pause. Requests within a Git sync remain sequential; the pause is between subscription syncs. Other hosts continue concurrently up to `poll.workers`.

```yaml
poll:
  workers: 4
  per_host_concurrency: 1
  per_host_delay: 2s
```

Due subscriptions remain queued rather than taking workers while their host is busy or cooling down. A long queue for one host cannot hide eligible subscriptions for other hosts. **Sync all**, **Sync Git feeds** and individual sync buttons make subscriptions due but cannot bypass host limits. The admin's Next sync column accounts for host cooldowns and shows **Waiting for host** while a due subscription's host is occupied. Git repository discovery from the admin also waits for the shared host slot, within `poll.timeout`.

HTTP 429 pauses the whole host group, using Retry-After or a one-minute fallback. Exhausted `X-RateLimit-Remaining` / `RateLimit-Remaining` budgets use the corresponding reset timestamp; HTTP 403/503 with Retry-After also pause the group. These cooldowns persist in SQLite across restarts. An ordinary permission-denied 403 or a 503 without Retry-After does not pause other subscriptions. A Git sync stops issuing further API requests if its budget becomes exhausted partway through pagination. Waiting sources do not acquire sync errors or increment their failure counters.

RSS redirects reserve their target host group too. A busy redirect target defers the sync without a failure, avoiding cross-host lock deadlocks. Cancellation and shutdown release active and queued reservations. Locking applies within one roi process; run one process per database as documented above. This reduces burst load but does not reduce the total request count or replace upstream API credentials.

Host cooldown handling follows the [GitHub REST rate-limit headers](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) and supports both forms of [HTTP Retry-After](https://www.rfc-editor.org/rfc/rfc9110.html#name-retry-after).


### Instance lock and build identity

Only one roi application can use a database at a time. An OS advisory lock on `<database>.roi.lock`, next to the canonical database path, prevents a second scheduler, including access through a symlink. The file may remain after shutdown; the kernel lock is released on graceful exit or a crash. All instances sharing a database must share that directory and use a filesystem that supports advisory locks.

`roi version` prints the version, commit, build time and Go version. The admin header shows the version with full metadata in its tooltip. RSS and Git requests identify the build as `roi/<version>` without a repository link. `just build`, artifact builds and Docker builds use `scripts/build.sh`; `ROI_VERSION`, `ROI_COMMIT` and `ROI_BUILD_DATE` can explicitly set metadata (including a reproducible UTC build time). Direct `go run`/`go build` uses development metadata when no release version is supplied.

### Resetting one feed

Each RSS and Git subscription has a **Reset feed** action (eraser icon) with a confirmation dialog. **Reset and resync** permanently removes that feed's downloaded articles, read/starred state, sync history and rotation tombstones. HTTP cache validators, errors and sync statistics are cleared. The feed keeps its ID, URL, name, folders, User-Agent and Git source options; it is enabled, unpaused and scheduled for download again. New articles receive new IDs. Only articles still available at the source can return. Reader clients may retain old cached copies.

Reset is an authenticated, CSRF-protected admin action and requires explicit confirmation. It is transactional, affects only the selected subscription and invalidates in-flight fetch results. Shared host cooldowns and per-host concurrency/delays remain in force; resetting a feed does not bypass a remote API rate limit.

### Import preview and sync diagnostics

In the admin, OPML and ROI uploads first open an import preview. It lists additions, existing/within-file duplicates and invalid records, and makes no database changes. HTTP/HTTPS variants count as duplicates. Confirmation applies the file atomically with validation repeated against the current database. Invalid records block confirmation. Preview and import share normalization and validation rules; malformed files and unknown formats are rejected.

Sync failures retain their source detail with a category: DNS, TLS, Timeout, HTTP, Parse, Storage, Network or Size limit. The feed's next-sync cell distinguishes another active subscription, a configured per-host delay, HTTP 429, an exhausted API quota and Retry-After cooldowns, with the resume time when known. Persisted cooldowns restored after restart use the general Rate limit / Retry-After reason. `roi_fetch_failures_total{category="..."}` counts categorized failures; existing counters remain available.

### Git request cache and development checks

If a Git API or Gitweb response has a corrupt gzip header, DEFLATE stream, checksum or truncated compressed body, roi retries that GET once with `Accept-Encoding: identity`. Both attempts share the request deadline and the operation's download budget, including bytes decoded before the first failure. Failed responses never enter the HTTP cache. A server that still sends compressed data on the fallback request is reported as a Network error with its URL. HTTP errors, ordinary read errors and exhausted download limits do not trigger this retry. `roi_git_identity_retries_total` counts fallback attempts.

Git tokens are sent only to HTTPS endpoints. HTTP repositories without credentials remain supported. Git list and detail requests use ETag/Last-Modified conditional GETs; a 304 reuses the saved body and pagination metadata. Cache entries are isolated by URL, credential digest and feed revision, stored in SQLite for at most seven days, and bounded to 2048 entries and 128 MiB of body, key, header and per-record accounting in total. ETag is limited to 4096 bytes and Last-Modified to 128 bytes; responses with larger validators are not cached. Outbound HTTP response headers are capped at 64 KiB. Cached bodies still count toward the per-operation processing budget. Credentials themselves are not stored in the cache. `roi_git_http_cache_hits_total` counts successful conditional-cache hits.

`just fuzz 15s` exercises Reader item IDs, OPML/ROI decoding and Gitweb conversion independently. `just benchmark` measures metadata reads with 10,000 archived articles containing roughly 40 MiB of text; setup is excluded from timing. CI runs bounded parser fuzzing and a benchmark smoke check in addition to race, coverage, browser and static-binary checks.

Public configuration examples must not contain real tokens. Keep private settings in ignored `config.yaml` or `examples/*.local.yaml`, for example `just run examples/config.local.yaml`. The default `just run` continues to use the sanitized `examples/config.yaml`. The original reference `worker.js` is local-only and excluded from version control.

### Additional security controls

API token signatures are verified before querying registered sessions. The global API request budget applies before authentication, in addition to the per-IP budget. Repeated 401/403/429 audit events from the same realm/IP/status/category are coalesced for 30 seconds; login failures have a separate category from denied requests. Denial writes have a global two-per-second budget with a burst of 20. Successful logins, device approvals, visibility changes and revocations are retained independently. `roi_security_events_suppressed_total` exposes coalesced/rate-limited denials. Debug request logs remain available when explicitly enabled.

`security.file_permissions` defaults to `warn`; `strict` rejects configuration and existing database/WAL/SHM files that allow group/other access, and rejects an accessible data directory. Use 0600 for private files and 0700 for the database directory. Existing permissions are never silently changed. Public test examples intentionally use warning mode. Normal session persistence and permanent approved Reader tokens are unchanged. Both `rotate-api-key` and `restore` take the same exclusive instance lock as the server; stop roi before using them.

`security.parse_concurrency` defaults to 2 and `security.parse_memory_bytes` to 128 MiB. The shared admission gate reserves twice `poll.max_bytes` for a feed/Git operation and 16 MiB for subscription imports. These are conservative admission weights, not an exact heap-size guarantee; keep an OS/container memory limit as the outer bound. Waiting for parser and password-hash slots is cancelable. XML/JSON nesting is capped at 128, structured document tokens at 200,000, feed articles at 10,000 and XML attributes per element at 64. HTML tokenizer buffers are capped at 4 MiB. YAML has a lexical width limit, 300,000 syntax nodes, depth 64 and no aliases. Argon2/bcrypt operations already in progress finish within the configured hash work factor.

`poll.allowed_networks` accepts exact IPv4/IPv6 addresses or non-default-route CIDRs, allowing private Git/RSS servers without opening all internal networks. Public addresses remain available. Link-local and known cloud metadata addresses are denied even if explicitly listed or if the legacy `allow_private_networks` override is enabled. DNS results and redirects use the same checks; mapped, NAT64 and 6to4 addresses are normalized before checking.

### Choosing the real client IP header

`security.real_ip_header` selects the trusted proxy's client-address header, similar to nginx's `real_ip_header`. Empty preserves the existing X-Forwarded-For chain with X-Real-IP fallback. Explicit `X-Forwarded-For` uses only that chain; other configured names accept exactly one IPv4/IPv6 address. Missing, repeated or malformed selected headers fall back to the socket peer, never to a different header. Header names are case-insensitive. No forwarding header is trusted unless the immediate peer belongs to `security.trusted_proxies`.

For an already trusted local nginx/cloudflared proxy that supplies a verified Cloudflare client header:

```yaml
security:
  real_ip_header: CF-Connecting-IP
  trusted_proxies: ['127.0.0.1', '::1']
```

For direct Cloudflare connections, populate `trusted_proxies` with the applicable [official Cloudflare IPv4/IPv6 networks](https://www.cloudflare.com/ips/). A local proxy must overwrite or validate the selected header; do not forward a user-supplied CF-Connecting-IP from arbitrary direct traffic. See [Cloudflare request headers](https://developers.cloudflare.com/fundamentals/reference/http-headers/) and [nginx realip](https://nginx.org/en/docs/http/ngx_http_realip_module.html). Resolved addresses are used consistently for sessions, audit events, request logs and per-IP limits.

### Hide and show feeds for one Reader device

Approved Reader devices have **Hide feeds / Show feeds** eye buttons next to their session actions. Hiding returns empty subscription, folder, unread, ID and article lists to that device and blocks its mutations, while its token, device name and approval remain valid. Other devices, stored articles, IDs and read/starred state are unaffected. The setting persists across restarts and changes are recorded in Security events.

To request a refreshed client subscription list, hide feeds, sync Reeder until it observes the empty account, then show feeds and sync again. This does not change article IDs and does not manufacture duplicate server entries. Whether old content is evicted from the local cache remains a client decision; the server cannot guarantee a Reeder cache reset.
