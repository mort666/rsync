/*
 * tokharness: drive upstream rsync's token.c on its own, so its compressed
 * token stream can be checked against another implementation.
 *
 * Built against an upstream rsync tree that has been configured with
 * --with-included-zlib=yes (see Makefile). token.c is linked unmodified;
 * this file only stubs the I/O, logging and memory helpers it calls.
 *
 *   tokharness send <algo> <proto> <level> <newfile> <opsfile>  > stream
 *   tokharness recv <algo> <proto> <basis> <blocklen>  < stream > out
 *
 * algo is none, zlib or zlibx, and lz4 or zstd where token.c was built with
 * them.
 *
 * The ops file holds one send_token() call per line, "tok n toklen", exactly
 * as match.c:matched() would make them for the new file.
 */
#include "rsync.h"

int do_compression = CPRES_NONE;
int do_compression_level = 6;
int do_compression_threads = 0;
int protocol_version = 31;
int module_id = -1;
char *skip_compress = NULL;

static char *mapped;
static OFF_T mapped_len;

NORETURN void _exit_cleanup(int code, const char *file, int line)
{
  fprintf(stderr, "tokharness: exit_cleanup(%d) from %s:%d\n", code, file, line);
  exit(code ? code : 1);
}

void rprintf(enum logcode code, const char *format, ...)
{
  va_list ap;
  (void)code;
  va_start(ap, format);
  vfprintf(stderr, format, ap);
  va_end(ap);
}

void *my_alloc(void *ptr, size_t num, size_t size, const char *file, int line)
{
  void *p = realloc(ptr, num * size);
  if (!p) {
    fprintf(stderr, "out of memory at %s:%d\n", file, line);
    exit(1);
  }
  return p;
}

char *lp_dont_compress(int mod)
{
  (void)mod;
  return "";
}

char *map_ptr(struct map_struct *map, OFF_T offset, int32 len)
{
  (void)map;
  if (offset < 0 || len < 0 || offset + len > mapped_len) {
    fprintf(stderr, "map_ptr out of range: off=%lld len=%d size=%lld\n",
      (long long)offset, (int)len, (long long)mapped_len);
    exit(1);
  }
  return mapped + offset;
}

void read_buf(int f, char *buf, size_t len)
{
  (void)f;
  if (len && fread(buf, 1, len, stdin) != len) {
    fprintf(stderr, "short read\n");
    exit(1);
  }
}

uchar read_byte(int f)
{
  uchar c;
  read_buf(f, (char *)&c, 1);
  return c;
}

int32 read_int(int f)
{
  char b[4];
  read_buf(f, b, 4);
  return IVAL(b, 0);
}

void write_buf(int f, const char *buf, size_t len)
{
  (void)f;
  if (len && fwrite(buf, 1, len, stdout) != len) {
    fprintf(stderr, "short write\n");
    exit(1);
  }
}

void write_byte(int f, uchar c)
{
  write_buf(f, (char *)&c, 1);
}

void write_int(int f, int32 x)
{
  char b[4];
  SIVAL(b, 0, x);
  write_buf(f, b, 4);
}

static char *slurp(const char *path, OFF_T *len)
{
  FILE *fp = fopen(path, "rb");
  char *buf;
  long n;
  if (!fp) {
    perror(path);
    exit(1);
  }
  fseek(fp, 0, SEEK_END);
  n = ftell(fp);
  fseek(fp, 0, SEEK_SET);
  buf = malloc(n ? n : 1);
  if (n && fread(buf, 1, n, fp) != (size_t)n) {
    perror(path);
    exit(1);
  }
  fclose(fp);
  *len = n;
  return buf;
}

static int algo(const char *s)
{
  if (strcmp(s, "none") == 0)
    return CPRES_NONE;
  if (strcmp(s, "zlib") == 0)
    return CPRES_ZLIB;
  if (strcmp(s, "zlibx") == 0)
    return CPRES_ZLIBX;
#ifdef SUPPORT_LZ4
  if (strcmp(s, "lz4") == 0)
    return CPRES_LZ4;
#endif
#ifdef SUPPORT_ZSTD
  if (strcmp(s, "zstd") == 0)
    return CPRES_ZSTD;
#endif
  fprintf(stderr, "unknown algorithm %s\n", s);
  exit(2);
}

static int do_send(char **argv)
{
  FILE *ops;
  long tok, n, toklen;
  OFF_T pos = 0;

  do_compression_level = atoi(argv[4]);
  /* Sets token.c's per_file_default_level, which deflateInit2() uses.
   * Without this, upstream would deflate at level 0 (stored blocks). */
  set_compression(argv[5]);
  mapped = slurp(argv[5], &mapped_len);
  if (!(ops = fopen(argv[6], "r"))) {
    perror(argv[6]);
    return 1;
  }
  while (fscanf(ops, "%ld %ld %ld", &tok, &n, &toklen) == 3) {
    send_token(1, (int32)tok, NULL, pos, (int32)n, (int32)toklen);
    pos += n + (tok >= 0 ? toklen : 0);
  }
  fclose(ops);
  fflush(stdout);
  if (pos != mapped_len) {
    fprintf(stderr, "ops cover %lld bytes, file is %lld\n", (long long)pos, (long long)mapped_len);
    return 1;
  }
  return 0;
}

static int do_recv(char **argv)
{
  int32 blen = atoi(argv[5]);
  int32 count, remainder, i, len;
  char *data;

  mapped = slurp(argv[4], &mapped_len);
  count = (int32)((mapped_len + blen - 1) / blen);
  remainder = (int32)(mapped_len % blen);

  while ((i = recv_token(0, &data)) != 0) {
    if (i > 0) {
      fwrite(data, 1, i, stdout);
      continue;
    }
    i = -(i + 1);
    if (i < 0 || i >= count) {
      fprintf(stderr, "bad block index %d (count %d)\n", (int)i, (int)count);
      return 1;
    }
    len = (i == count - 1 && remainder) ? remainder : blen;
    data = map_ptr(NULL, (OFF_T)i * blen, len);
    see_token(data, len);
    fwrite(data, 1, len, stdout);
  }
  fflush(stdout);
  return 0;
}

int main(int argc, char **argv)
{
  if (argc == 7 && strcmp(argv[1], "send") == 0) {
    do_compression = algo(argv[2]);
    protocol_version = atoi(argv[3]);
    return do_send(argv);
  }
  if (argc == 6 && strcmp(argv[1], "recv") == 0) {
    do_compression = algo(argv[2]);
    protocol_version = atoi(argv[3]);
    return do_recv(argv);
  }
  fprintf(stderr,
    "usage: tokharness send <algo> <proto> <level> <newfile> <opsfile> > stream\n"
    "       tokharness recv <algo> <proto> <basis> <blocklen> < stream > out\n");
  return 2;
}
