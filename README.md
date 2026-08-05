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

`times` in the setup JSON or `DOTIME` in the setup URL limits how many matched
requests receive the response. When omitted, the setup remains until it is
overwritten or reset. Setups are held in memory and are lost when the process
restarts.

Only `response.body` is rendered as a Go `text/template`. Status and headers
are fixed at setup time. The template receives:

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