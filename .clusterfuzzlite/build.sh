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
#
# The Go builder's LIB_FUZZING_ENGINE links gosigfuzz.o, whose LLVMFuzzerInitialize
# adds SA_ONSTACK to the signal handlers (Go runs signal handlers on an alternate
# stack). There can be only one LLVMFuzzerInitialize, so goinit.o does both and
# replaces gosigfuzz.o in the link.
cat > "$WORK/goinit.c" <<'EOF'
#include <signal.h>
#include <stddef.h>
#include <stdint.h>

extern uintptr_t _cgo_wait_runtime_init_done(void);

/* As OSS-Fuzz's gosigfuzz.c: keep the handler, add SA_ONSTACK. */
static void on_stack(int signum) {
	struct sigaction act;
	if (sigaction(signum, NULL, &act) == 0) {
		act.sa_flags |= SA_ONSTACK;
		sigaction(signum, &act, NULL);
	}
}

int LLVMFuzzerInitialize(int *argc, char ***argv) {
	static const int signals[] = {SIGSEGV, SIGABRT, SIGALRM, SIGINT, SIGTERM,
		SIGBUS, SIGFPE, SIGXFSZ, SIGUSR1, SIGUSR2};
	(void)argc;
	(void)argv;
	_cgo_wait_runtime_init_done();
	for (unsigned i = 0; i < sizeof signals / sizeof signals[0]; i++) {
		on_stack(signals[i]);
	}
	return 0;
}
EOF
# shellcheck disable=SC2086 # CFLAGS holds several flags.
$CC $CFLAGS -c "$WORK/goinit.c" -o "$WORK/goinit.o"
engine=()
for flag in $LIB_FUZZING_ENGINE; do
	case "$flag" in
	*/gosigfuzz.o) ;;
	*) engine+=("$flag") ;;
	esac
done

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
	# shellcheck disable=SC2086 # CXXFLAGS holds several flags.
	$CXX $CXXFLAGS "${engine[@]}" "$name.a" "$WORK/goinit.o" -o "$OUT/$name"
done
