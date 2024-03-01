Osbuild-as-a-service
--------------------

Day of learning trivial go webservice.

To test:
```
$ go test ./...
```
to run
```
$ cd cmd/oaas/
$ go build && sudo ./oaas -builddirbase /var/tmp/my-build
```
then from the client:
```
$ echo '{"exports": ["image"]}' > control.json
$ cp /path/to/you/manifest.json manifest.json
$ tar cvf test.tar control.json manifest.json
$ curl -o - --data-binary "@./test.tar" -H "Content-Type: application/x-tar"  -X POST http://localhost:8001/api/v1/build
[output]
$ curl -o disk.img http://localhost:8001/api/v1/result/image/disk.img
```

