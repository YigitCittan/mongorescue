# ClusterFuzzLite build image (.github/workflows/cflite.yml). The OSS-Fuzz Go
# builder brings clang, libFuzzer, Go and compile_native_go_fuzzer; build.sh turns
# the native Go fuzz targets of the Makefile's FUZZ_TARGETS into libFuzzer binaries.
FROM gcr.io/oss-fuzz-base/base-builder-go:latest@sha256:0ebd94f573c7917b93a7edc8cd1db21c167adbd1d9c59943c3ebe4afb7aa4497
COPY . $SRC/mongorescue
WORKDIR $SRC/mongorescue
COPY .clusterfuzzlite/build.sh $SRC/
