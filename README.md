# pathecho

HTTP stub server for testing. By default it answers any path with
method-appropriate responses (GET echoes the request URI; POST/PUT/DELETE
return standard success codes). You can also configure per-path, per-method
stub responses with Go templates, hit limits, response delays, and reset controls.

## Project layout

- `cmd/pathecho`: executable startup, logging, and TLS configuration
- `internal/server`: HTTP router and default request behavior
- `internal/stub`: configurable stub response engine
- `internal/oauth`: in-memory OAuth/OIDC test provider
- `internal/httpapi`: shared HTTP control and JSON helpers
- `e2e`: container-backed end-to-end tests that also double as usage examples

## Configure stub responses

Use a `POST` request with `DO=setup` to configure a response for a method and
path. Query parameters and the request body on later requests are available to
the template but are not part of response matching.

Prefer a JSON object (or array) for `response.body` so you do not have to
escape the entire payload as a string. Each string value in that object is
rendered as its own template; if the result is valid JSON (number, object,
array, boolean, or null), it is inserted as that JSON value. Use `.Q` / `.H`
for simple keys, and backticks when a key is not a valid identifier:

```shell
curl -X POST 'http://localhost:9095/users?DO=setup&DOTIME=10' \
  -H 'Content-Type: application/json' \
  -d '{
    "method": "GET",
    "response": {
      "status": 200,
      "headers": {"Content-Type": "application/json"},
      "body": {
        "name": "{{.Q.name}}",
        "age": "{{add (parseInt .Q.age) 10}}",
        "auth": "{{.H.Authorization}}",
        "userId": "{{index .Q `user-id`}}",
        "servedAt": "{{formatTime `2006-01-02T15:04:05Z07:00` .Now}}"
      }
    }
  }'
```

All-digit values such as `42` become JSON numbers. To keep one as a string,
wrap it with `jsonString`: `"{{jsonString (index .Q `user-id`)}}"`.

The configured response is matched by method and path:

```shell
curl 'http://localhost:9095/users?name=Sam&age=30&user-id=u-42'
```

Echo fields from a JSON request body with `jsonPath` (`.Body` is always the
raw body; `.J` is set when that body is valid JSON):

```shell
curl -X POST 'http://localhost:9095/users?DO=setup' \
  -H 'Content-Type: application/json' \
  -d '{
    "method": "POST",
    "response": {
      "status": 200,
      "headers": {"Content-Type": "application/json"},
      "body": {
        "name": "{{jsonPath \"$.user.name\" .J}}",
        "age": "{{jsonPath \"$.user.age\" .J}}",
        "raw": "{{.Body}}"
      }
    }
  }'

curl -X POST 'http://localhost:9095/users' \
  -H 'Content-Type: application/json' \
  -d '{"user":{"name":"Sam","age":30}}'
```

Response header values use the same templates. For example, configure a
dynamic redirect:

```shell
curl -X POST 'http://localhost:9095/authorize?DO=setup' \
  -H 'Content-Type: application/json' \
  -d '{
    "method": "GET",
    "response": {
      "status": 302,
      "headers": {
        "Location": "{{.Q.redirect_uri}}?code=test-code&state={{.Q.state}}"
      }
    }
  }'
```

`times` in the setup JSON or `DOTIME` in the setup URL limits how many matched
requests receive the response. When omitted, the setup remains until it is
overwritten or reset. Setups are held in memory and are lost when the process
restarts. The server stores at most 1,024 configured responses; setup request
bodies and rendered response bodies/headers are limited to 1 MiB.

`delays` in the setup JSON waits before returning a matched response, useful for
mimicking slow dependencies. Values are milliseconds:

- number or digit string: fixed wait (`150` or `"150"`)
- `R<max>`: uniform random wait in `[0, max]` (`"R50"`)
- `R<min>-<max>`: uniform random wait in `[min, max]` (`"R20-80"`)
- array of the above: cycle one entry per hit (`["5", 10, "R20-80"]`)

Omit `delays`, or use `0` / `"0"`, for no wait. Each value must be an integer
from 0 through 30000 (30 seconds).

String values in `response.body` and response header values are rendered as Go
`text/template` templates. Status is fixed at setup time. The template receives:

- `.Method`, `.Path`, `.Query`, `.Header`, `.Q`, `.H`, `.Body`, `.J`, and `.Now`
- `.Q` / `.H`: first-value maps for query params and request headers
  (`{{.Q.age}}`, `{{.H.Authorization}}`). Missing keys render as empty.
  Keys that are not valid identifiers (for example `user-id`) need
  `index` with backticks, such as `{{index .Q `user-id`}}`.
- `.Body`: raw request body as a string (any content type)
- `.J`: parsed JSON value when `.Body` is valid JSON; otherwise empty/nil
- `jsonPath`: JSONPath helper for `.J` (or a JSON string), for example
  `{{jsonPath "$.user.name" .J}}` or `{{.J | jsonPath "$.user.age"}}`.
  Missing matches render as empty. A single string match is returned as text;
  other values (and multi-matches) are returned as JSON text so object
  response bodies can insert them as typed JSON.
- Numeric helpers: `add`, `sub`, `mul`, `div`, `mod`, `abs`, `min`, `max`,
  `pow`, `sqrt`, `log`, `round`, `floor`, `ceil`, `parseInt`, and `parseFloat`
- String helpers: `lower`, `upper`, `trim`, `contains`, `replace`, `split`,
  `join`, and `default`
- Time helpers: `now`, `formatTime`, `addTime`, and `unix`
- JSON helpers: `toJSON` and `jsonString` (mainly useful when `body` is a
  single template string rather than a JSON object)

Templates are validated when configured. File, environment, network, shell,
reflection, and arbitrary Go package access are not exposed.

### Reset responses

Reset one method on a path:

```shell
curl -X POST 'http://localhost:9095/users?DO=reset' \
  -H 'Content-Type: application/json' \
  -d '{"method":"GET"}'
```

Use an empty body to reset every configured method on that path:

```shell
curl -X POST 'http://localhost:9095/users?DO=reset'
```

Reset all configured responses:

```shell
curl -X POST 'http://localhost:9095/RESET'
```

## Test OAuth provider

Pathecho can run an in-memory OAuth 2.0/OIDC test provider. The `/oauth/*`
paths are reserved for this feature. Configure it after startup:

```shell
curl -X POST 'http://localhost:9095/oauth?DO=setup' \
  -H 'Content-Type: application/json' \
  -d '{
    "issuer": "http://localhost:9095/oauth",
    "audience": "test-api",
    "tokenTTL": "1h",
    "defaultUser": "alice",
    "claims": {"tenant": "test"},
    "clients": {
      "test-client": {
        "secret": "test-secret",
        "redirectURIs": ["http://localhost:3000/callback"],
        "scopes": ["openid", "profile", "api.read"]
      }
    },
    "users": {
      "alice": {
        "password": "alice-password",
        "claims": {
          "sub": "user-alice",
          "email": "alice@example.com",
          "roles": ["admin"]
        }
      }
    }
  }'
```

The issuer must end in `/oauth`. Setup generates an in-memory RSA signing key
unless `privateKeyPEM` supplies a PKCS#1 or PKCS#8 RSA private key. Setup again
replaces the configuration, rotates generated keys, and invalidates existing
codes and refresh tokens. Imported RSA keys must be at least 2,048 bits.

The provider exposes:

- `GET /oauth/.well-known/openid-configuration`
- `GET /.well-known/oauth-authorization-server/oauth`
- `GET /oauth/jwks`
- `GET /oauth/authorize`
- `POST /oauth/token`

All four supported grants use form-encoded token requests. Client credentials:

```shell
curl -X POST 'http://localhost:9095/oauth/token' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=client_credentials&client_id=test-client&client_secret=test-secret&scope=api.read'
```

Password grant (legacy and intended only for tests):

```shell
curl -X POST 'http://localhost:9095/oauth/token' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=password&client_id=test-client&client_secret=test-secret&username=alice&password=alice-password&scope=openid profile'
```

Authorization code requests require a registered redirect URI. There is no
login UI: `login_hint` selects a configured user, or `defaultUser` is used,
and the provider immediately redirects with a short-lived one-time code.
PKCE `plain` and `S256` are supported and required for clients without a
secret. Public clients cannot use the `client_credentials` or `password`
grants.

Exchange the returned code at `/oauth/token` with
`grant_type=authorization_code`, `code`, the same `redirect_uri`, and
`code_verifier` when PKCE was used. User grants return a refresh token when
the `refresh_token` grant is enabled:

```shell
curl -X POST 'http://localhost:9095/oauth/token' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=refresh_token&client_id=test-client&client_secret=test-secret&refresh_token=<token>'
```

The enabled grants default to `authorization_code`, `client_credentials`,
`refresh_token`, and `password`. Use `enabledGrants` during setup to restrict
that list; it cannot be empty. A client's `scopes` value is an allow-list
(`["*"]` allows any requested scope). Reset the provider and discard all
in-memory keys and tokens with:

```shell
curl -X POST 'http://localhost:9095/oauth?DO=reset'
```

This provider is for testing only. Users, plaintext test passwords, clients,
keys, authorization codes, and refresh tokens are not persisted.


# To use pathecho in k8s env with tls on
Create a tls secret, then use the secret when configures the
pod:

```
##################################################################################################
# Pathecho service @ port 31056
##################################################################################################
apiVersion: v1
kind: Service
metadata:
  name: pathecho-31056
spec:
  ports:
  - port: 31056
    targetPort: 8080
    name: port31056
  selector:
    app: pathecho-31056
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pathecho-31056
  labels:
    app: pathecho-31056
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pathecho-31056
  template:
    metadata:
      labels:
        app: pathecho-31056
    spec:
      volumes:
        - name: tlskeys
          secret:
            secretName: <secret name>
      containers:
      - name: pathecho
        image: docker.io/email4tong/pathecho:v1.0.0
        imagePullPolicy: Always
        ports:
        - containerPort: 8080
        env:
        - name: TLS_CERT
          value: "/etc/mytls/tls.crt"
        - name: TLS_KEY
          value: "/etc/mytls/tls.key"
        volumeMounts:
        - name: tlskeys
          mountPath: "/etc/mytls"
          readOnly: true
```

# Run with Docker

```shell
docker run -dit -p 9095:8080 --rm email4tong/pathecho
```

# Build container image:

```
docker build -t email4tong/pathecho .
```

# Run the tests

Unit tests (no Docker required):

```shell
go test ./...
```

Run with the race detector and vet:

```shell
go test -race ./...
go vet ./...
```

## End-to-end tests

The `e2e` package builds the Docker image, starts a container, then exercises
the same flows documented above (stub setup/templates/resets and the OAuth
provider grants). Comments in the test files include the equivalent `curl`
examples.

Requires Docker. The suite builds `pathecho-e2e:local` unless you point it at
an existing image:

```shell
go test -tags=e2e ./e2e/ -count=1 -v
```

Reuse an already-built image:

```shell
docker build -t email4tong/pathecho:local .
PATHECHO_E2E_IMAGE=email4tong/pathecho:local PATHECHO_E2E_SKIP_BUILD=1 \
  go test -tags=e2e ./e2e/ -count=1 -v
```

## Unit test coverage

Statement coverage per package (measured across the full suite with
`-coverpkg=./...`, so integration tests in `internal/server` count toward the
packages they exercise):

| Package | Coverage |
| --- | --- |
| `internal/httpapi` | 86.2% |
| `internal/oauth` | 79.8% |
| `internal/server` | 78.4% |
| `internal/stub` | 63.4% |
| `cmd/pathecho` | 3.3% |
| **Total** | **71.8%** |

`cmd/pathecho` is mostly process startup and TLS wiring, so it is intentionally
thin on unit coverage.

Reproduce the total and a per-function breakdown:

```shell
go test -coverpkg=./... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

Measure a single package (the suite still runs, but only that package's
statements are counted):

```shell
go test -coverpkg=./internal/oauth ./... -coverprofile=oauth.out
go tool cover -func=oauth.out | tail -1
```

View coverage highlighted in your browser:

```shell
go tool cover -html=coverage.out
```