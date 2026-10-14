# ClusterFuzzLite build image (.github/workflows/cflite.yml). The OSS-Fuzz Go
# builder brings clang, libFuzzer, Go and compile_native_go_fuzzer; build.sh turns
# the native Go fuzz targets of the Makefile's FUZZ_TARGETS into libFuzzer binaries.
FROM gcr.io/oss-fuzz-base/base-builder-go:latest@sha256:1dbadefbb28c6bfd796c64d9c2115977deb8f19494533cc5a3b724c3441df23d
COPY . $SRC/mongorescue
WORKDIR $SRC/mongorescue
COPY .clusterfuzzlite/build.sh $SRC/
