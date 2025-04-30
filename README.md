This is a port of inactive Hashicorp freeport library.

It's primary function is to reduce the initialization panics in the original implementation. We observed many flakey CI runs because of
`cannot allocate port block` https://github.com/hashicorp/consul/blob/f3c5d71cbf7944fa99df9bf4f33fc213223f170d/sdk/freeport/freeport.go#L274

The root cause is many concurrent memory spaces (ie concurrent `go test`) that collide because the hardcode block size is far too large for our use case, limiting the number of port blocks to ~ 25.

This migration drops depreciated functions and window-specific builds, since they are not relevant.