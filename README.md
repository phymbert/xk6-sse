# xk6-sse
A [k6](https://go.k6.io/k6) extension for [Server-Sent Events (SSE)](https://en.wikipedia.org/wiki/Server-sent_events) using the [xk6](https://github.com/grafana/xk6) system.

See the [K6 SSE Extension design](docs/design/021-sse-api.md).

## k6 version

This extension is tested with `k6` version `v1.0.0` last release is [v0.1.10](https://github.com/phymbert/xk6-sse/releases/tag/v0.1.10).

## Build

To build a `k6` binary with this plugin, first ensure you have the prerequisites:

- [Go toolchain](https://go101.org/article/go-toolchain.html)
- Git

Then:

1. Install [xk6](https://github.com/grafana/xk6):

```shell
go install go.k6.io/xk6/cmd/xk6@latest
```

2. Build the binary:

```shell
xk6 build master \
--with github.com/phymbert/xk6-sse
```

## Example

```javascript
import sse from "k6/x/sse"
import {check} from "k6"

export default function () {
    const url = "https://echo.websocket.org/.sse"
    const params = {
        method: 'GET',
        headers: {
            "Authorization": "Bearer XXXX"
        },
        tags: {"my_k6s_tag": "hello sse"}
    }

    const response = sse.open(url, params, function (client) {
        client.on('open', function open() {
            console.log('connected')
        })

        client.on('event', function (event) {
            console.log(`event id=${event.id}, name=${event.name}, data=${event.data}`)
            if (parseInt(event.id) === 4) {
                client.close()
            }
        })

        client.on('error', function (e) {
            console.log('An unexpected error occurred: ', e.error())
        })
    })

    check(response, {"status is 200": (r) => r && r.status === 200})
}
```

### License

                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/