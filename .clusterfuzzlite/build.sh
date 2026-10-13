#!/bin/bash -eu
# ==============================================================================
# ClusterFuzzLite build script: compiles every native Go fuzz target listed in the
# Makefile's FUZZ_TARGETS (make fuzz-list) into a libFuzzer binary in $OUT, named
# <package>_<FuzzFunc> (for example oplog_FuzzFilter).
#
# compile_native_go_fuzzer_v2 (go-118-fuzz-build v2) compiles all _test.go files of
# a target's package into the package itself, renamed to *_libFuzzer.go, so the
# targets can use their package's test helpers. (The plain compile_native_go_fuzzer
# is the legacy builder, which needs an extra module in go.mod.) Two kinds of test
# files cannot be compiled that way and are removed from a throwaway copy of the
# source, which also keeps the files the tool creates out of $SRC:
#
# - files of an external test package (package foo_test);
# - files for another operating system by name (foo_windows_test.go): the rename
#   drops the _windows suffix, so Go would compile them on Linux.
#
# In libFuzzer builds *testing.F is a stand-in without all of testing.TB, so seed
# helpers that fuzz targets call with f take a narrower interface (fataler, helperT).
# The seed corpora under testdata/ are read relative to the package directory by go
# test; the libFuzzer binaries run elsewhere, where reading them fails softly
# (f.Fatal is a no-op there) and the engine's corpus takes over.
# ==============================================================================

tree="${WORK:?}/mongorescue"
rm -rf "$tree"
cp -a "$SRC/mongorescue/." "$tree"
cd "$tree"

module=$(go list -m)

grep -rl --include='*_test.go' -E '^package [A-Za-z0-9_]+_test$' . | xargs -r rm -f
find . -name '*_test.go' -regextype posix-extended \
	-regex '.*_(aix|android|darwin|dragonfly|freebsd|illumos|ios|js|netbsd|openbsd|plan9|solaris|wasip1|windows)(_[a-z0-9]+)?_test\.go' \
	-delete

make -s fuzz-list | while IFS=: read -r pkg fn; do
	[ -n "$pkg" ] || continue
	name="$(basename "$pkg")_${fn}"
	echo "==> Building $name from $pkg"
	compile_native_go_fuzzer_v2 "${module}/${pkg#./}" "$fn" "$name"
done
