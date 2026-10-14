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
# In libFuzzer builds *testing.F is a stand-in that does not implement all of
# testing.TB and whose Fatal returns, and the whole Fuzz function runs for every
# input. So seed helpers called with f take a narrower interface (fataler,
# helperT); setup that fails panics after Fatal (fuzzFail) instead of leaving a nil
# value behind; seed files are optional (the binaries run away from testdata/, the
# engine's corpus takes over); and expensive seeds are built once (sync.OnceValues).
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

# Go builds each target as a c-archive, whose runtime initializes on a thread of its
# own while libFuzzer's main starts; the runtime registers the coverage counters
# (__sanitizer_cov_8bit_counters_init) during that initialization. libFuzzer could
# reach its first input before them and crash in TracePC::ClearInlineCounters on a
# half-registered module (SEGV at 0x10, without the "Loaded 1 modules" line; seen
# on mongotls_FuzzDecryptKey). LLVMFuzzerInitialize, which libFuzzer calls before it
# looks at the counters, waits for the Go runtime to finish initializing, as every
# call into an exported Go function does. Coverage builds (go test -c) are plain Go
# binaries and keep compile_native_go_fuzzer_v2.
cat > "$WORK/goinit.c" <<'EOF'
#include <stdint.h>

extern uintptr_t _cgo_wait_runtime_init_done(void);

int LLVMFuzzerInitialize(int *argc, char ***argv) {
	(void)argc;
	(void)argv;
	_cgo_wait_runtime_init_done();
	return 0;
}
EOF
# shellcheck disable=SC2086 # CFLAGS holds several flags.
$CC $CFLAGS -c "$WORK/goinit.c" -o "$WORK/goinit.o"

make -s fuzz-list | while IFS=: read -r pkg fn; do
	[ -n "$pkg" ] || continue
	name="$(basename "$pkg")_${fn}"
	echo "==> Building $name from $pkg"
	if [[ "$SANITIZER" == *coverage* ]]; then
		compile_native_go_fuzzer_v2 "${module}/${pkg#./}" "$fn" "$name"
		continue
	fi
	# What compile_native_go_fuzzer_v2 runs for these sanitizers, plus goinit.o.
	dir=$(go list -tags gofuzz -f '{{.Dir}}' "${module}/${pkg#./}")
	go-118-fuzz-build_v2 -tags gofuzz -o "$name.a" -func "$fn" "$dir"
	# shellcheck disable=SC2086 # CXXFLAGS and LIB_FUZZING_ENGINE hold several flags.
	$CXX $CXXFLAGS $LIB_FUZZING_ENGINE "$name.a" "$WORK/goinit.o" -o "$OUT/$name"
done
