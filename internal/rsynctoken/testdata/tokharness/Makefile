# Builds tokharness, upstream rsync's token.c on its own, for the
# differential tests in ../../harness_test.go.
#
# RSYNC_SRC must be an upstream rsync tree configured with
#   ./configure --with-included-zlib=yes
# and built once with "make rsync", so that config.h and zlib/*.o exist.
# With lz4 and zstd found by configure, the harness has them too.
#
#   make RSYNC_SRC=/path/to/rsync
#   RSYNC_TOKEN_HARNESS=$PWD/bin go test ./internal/rsynctoken/
RSYNC_SRC ?= $(error set RSYNC_SRC to a configured upstream rsync tree)

.RECIPEPREFIX = >

ZLIB_OBJS = deflate.o inffast.o inflate.o inftrees.o trees.o zutil.o adler32.o compress.o crc32.o
LIBS = $(shell grep -q 'define SUPPORT_LZ4 1' $(RSYNC_SRC)/config.h && echo -llz4) \
       $(shell grep -q 'define SUPPORT_ZSTD 1' $(RSYNC_SRC)/config.h && echo -lzstd)

all: bin/tokharness bin/tokharness-sysz

# token.c with rsync's bundled zlib, which has Z_INSERT_ONLY.
bin/tokharness: tokharness.c
> mkdir -p bin
> gcc -O2 -DHAVE_CONFIG_H -I$(RSYNC_SRC) -I$(RSYNC_SRC)/zlib -o $@ \
>   tokharness.c $(RSYNC_SRC)/token.c $(addprefix $(RSYNC_SRC)/zlib/,$(ZLIB_OBJS)) $(LIBS)

# The same against the system zlib, which has no Z_INSERT_ONLY: token.c
# then uses Z_SYNC_FLUSH and discards the output, as distribution builds of
# rsync do.
bin/tokharness-sysz: tokharness.c
> mkdir -p bin
> gcc -O2 -DHAVE_CONFIG_H -I$(RSYNC_SRC) -o $@ tokharness.c $(RSYNC_SRC)/token.c -lz $(LIBS)

clean:
> rm -rf bin

.PHONY: all clean
