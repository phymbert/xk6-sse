#!/bin/bash

set -eux

xk6 build --with github.com/phymbert/xk6-sse=.
for script in examples/*
do
   ./k6 run --vus 5 --duration 10s "${script}"
done