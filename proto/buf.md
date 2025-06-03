# Protobufs

This is the public protocol buffers API for [vsld](https://github.com/EuclidProtocol/vsld).

## Download

The `buf` CLI comes with an export command. Use `buf export -h` for details

#### Examples:

Download cosmwasm protos for a commit:
```bash
buf export buf.build/EuclidProtocol/vsld:${commit} --output ./tmp
```

Download all project protos:
```bash
buf export . --output ./tmp
```