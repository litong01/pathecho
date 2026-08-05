# pathecho

HTTP stub server for testing. By default it answers any path with
method-appropriate responses (GET echoes the request URI; POST/PUT/DELETE
return standard success codes). You can also configure per-path, per-method
stub responses with Go templates, hit limits, and reset controls.

## Configure stub responses

Use a `POST` request with `DO=setup` to configure a response for a method and
path. Query parameters on later requests are available to the template but are
not part of response matching.

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
restarts.

String values in `response.body` and response header values are rendered as Go
`text/template` templates. Status is fixed at setup time. The template receives:

- `.Method`, `.Path`, `.Query`, `.Header`, `.Q`, `.H`, and `.Now`
- `.Q` / `.H`: first-value maps for query params and request headers
  (`{{.Q.age}}`, `{{.H.Authorization}}`). Missing keys render as empty.
  Keys that are not valid identifiers (for example `user-id`) need
  `index` with backticks, such as `{{index .Q `user-id`}}`.
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
codes and refresh tokens.

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
PKCE `plain` and `S256` are supported.

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