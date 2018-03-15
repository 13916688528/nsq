#!/bin/bash
set -e
echo "mode: atomic" > coverage.txt
echo "" > go_test.txt

if [ "$TEST_RACE" = "false" ]; then
    GOMAXPROCS=1 go test -timeout 900s `go list ./... | grep -v consistence | grep -v nsqadmin`
else
    for d in $(go list ./... | grep -v consistence | grep -v nsqadmin); do
        GOMAXPROCS=4 go test -timeout 900s -race -coverprofile=profile.out $d | tee -a go_test.txt
        if [ -f profile.out ]; then
            cat profile.out | grep -v "mode: atomic" >> coverage.txt
            rm profile.out
        fi
    done
fi

# no tests, but a build is something
for dir in $(find apps bench -maxdepth 1 -type d) nsqadmin; do
    if grep -q '^package main$' $dir/*.go 2>/dev/null; then
        echo "building $dir"
        go build -o $dir/$(basename $dir) ./$dir
    else
        echo "(skipped $dir)"
    fi
done
