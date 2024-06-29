# BFD

Experimental Bidirectional Forwarding Detection implementation in Go. Incomplete and probably very wrong.

Partially implements:

* [RFC 5880 - Bidirectional Forwarding Detection (BFD)](https://datatracker.ietf.org/doc/html/rfc5880)
* [RCF 5881 - Bidirectional Forwarding Detection (BFD) for IPv4 and IPv6 (Single Hop)](https://datatracker.ietf.org/doc/html/rfc5881)

This package will negotiate a BFD session with any directly connected peer that attempts to connect.

Only one session per peer is supported currently.

## Example usage

```
func main() {
    ip := netip.MustParseAddr(os.Args[1])

    bfd := bfd.BFD{}
    err := bfd.Start()

    if err != nil {
	log.Fatal("Start: ", err)
    }

    for {
        fmt.Println(ip, bfd.Query(ip))
	time.Sleep(time.Second)
    }
}
```

If you were running this on 10.1.2.3 as `go run main.go 10.1.2.4`, and on host 10.1.2.4 you set BIRD up with:

```
protocol bfd {
    neighbor 10.1.2.3;
}
```

then you should see a BFD session come up:

```
root:~/bfd/cmd# go run main.go 10.1.2.4
10.1.2.4 false
10.1.2.4 true
10.1.2.4 true
10.1.2.4 true
...
```
