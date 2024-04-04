# xk6-sse
A [k6](https://go.k6.io/k6) extension for [Server-Sent Events (SSE)](https://en.wikipedia.org/wiki/Server-sent_events) using the [xk6](https://github.com/grafana/xk6) system.


| /!\ This is a proof of concept, isn't supported by the k6 team, and may break in the future. USE AT YOUR OWN RISK! |
|--------------------------------------------------------------------------------------------------------------------|

See the [K6 SSE Extension design](docs/design/021-sse-api.md).

## k6 version

This extension is tested with `k6` version `v0.50.0` last release is [v0.1.0](https://github.com/phymbert/xk6-sse/releases/tag/v0.1.0).

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
import sse from "k6/x/sse";
import {check} from "k6";

export default function () {
    var url = "https://echo.websocket.org/.sse";
    var params = {"tags": {"my_tag": "hello"}};

    var response = sse.open(url, params, function (client) {
        client.on('open', function open() {
            console.log('connected');
        });

        client.on('event', function (event) {
            console.log(`event id=${event.id}, name=${event.name}, data=${event.data}`);
            if (parseInt(event.id) === 10) {
                client.close();
            }
        });

        client.on('error', function (e) {
            console.log('An unexpected error occurred: ', e.error());
        });
    });

    check(response, {"status is 200": (r) => r && r.status === 200});
};
```
