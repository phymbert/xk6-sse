#!/bin/bash

xk6 build --with github.com/phymbert/xk6-sse=. && ./k6 run --vus 5 --duration 10s examples/sse.js