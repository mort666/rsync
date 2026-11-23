package rsyncopts

import (
	"fmt"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

func (o *Options) CommandOptions(path string, paths ...string) []string {
	return append(o.ServerOptions(), append([]string{".", path}, paths...)...)
}

// rsync/options.c:server_options
func (o *Options) ServerOptions() []string {
	var sargv []string

	// if (blocking_io == -1)
	// 	blocking_io = 0;

	sargv = append(sargv, "--server")

	// if (daemon_over_rsh) {
	// 	args[ac++] = "--daemon";
	// 	*argc = ac;
	// 	/* if we're passing --daemon, we're done */
	// 	return;
	// }

	if !o.Sender() {
		sargv = append(sargv, "--sender")
	}

	argstr := "-"

	// One v per level, up to nine.
	for i := 0; i < o.verbose && i < 9; i++ {
		argstr += "v"
	}

	// /* the -q option is intentionally left out */
	// if (make_backups)
	// 	argstr[x++] = 'b';
	if o.UpdateOnly() {
		argstr += "u"
	}
	if o.DryRun() {
		argstr += "n"
	}
	if o.PreserveLinks() {
		argstr += "l"
	}
	// A sending client keeps its own -L and -k and the far end keeps the
	// folder links it has; a receiving one asks the sender to follow.
	if o.Sender() {
		if o.KeepDirlinks() {
			argstr += "K"
		}
	} else {
		if o.CopyLinks() {
			argstr += "L"
		}
		if o.CopyDirlinks() {
			argstr += "k"
		}
	}

	if o.whole_file > 0 && o.append_mode == 0 {
		argstr += "W"
	}
	// /* We don't need to send --no-whole-file, because it's the
	//  * default for remote transfers, and in any case old versions
	//  * of rsync will not understand it. */

	if o.PreserveHardLinks() {
		argstr += "H"
	}
	if o.PreserveUid() {
		argstr += "o"
	}
	if o.PreserveGid() {
		argstr += "g"
	}
	if o.PreserveDevices() {
		argstr += "D"
	}
	if o.PreserveMTimes() {
		argstr += "t"
	}
	if o.PreservePerms() {
		argstr += "p"
	}
	// -XX for rsync's own names too (options.c: X once per level, up
	// to two).
	for i := 0; i < o.preserve_xattrs && i < 2; i++ {
		argstr += "X"
	}
	if o.Recurse() {
		argstr += "r"
	}
	if o.AlwaysChecksum() {
		argstr += "c"
	}
	// if (cvs_exclude)
	// 	argstr[x++] = 'C';
	if o.IgnoreTimes() {
		argstr += "I"
	}
	// if (relative_paths)
	// 	argstr[x++] = 'R';
	// One x per -x, up to two.
	for i := 0; i < o.one_file_system && i < 2; i++ {
		argstr += "x"
	}
	if o.SparseFiles() {
		argstr += "S"
	}
	// rsync sends z for zlib, or for a compressor still to be negotiated;
	// other choices go as long options below.
	compress := o.CompressChoice()
	if compress == "auto" || compress == "zlib" {
		argstr += "z"
	}

	if o.xfer_dirs > 0 && !o.Recurse() {
		argstr += "d"
	}
	// Only a receiving server sets times, so only it gets -O.
	if o.Sender() && o.OmitDirTimes() {
		argstr += "O"
	}

	// From protocol 30 the client's capabilities ride on the end of the
	// short options as -e (options.c:maybe_add_e_option). Like rsync, this
	// looks at the version the client will announce: --protocol=29 leaves
	// it out.
	if o.ProtocolVersion() >= 30 {
		argstr += rsyncwire.ClientCapabilities
	}

	// argstr[x] = 0;

	if argstr != "-" {
		sargv = append(sargv, argstr)
	}

	if o.checksum_choice != "" {
		sargv = append(sargv, "--checksum-choice="+o.checksum_choice)
	}

	// options.c:server_options
	if compress != "" && o.CompressLevel() >= 0 {
		sargv = append(sargv, fmt.Sprintf("--compress-level=%d", o.CompressLevel()))
	}
	switch {
	case compress == "zlibx":
		sargv = append(sargv, "--new-compress")
	case compress == "zlib" && o.compress_choice != "":
		sargv = append(sargv, "--old-compress")
	case compress != "" && compress != "auto" && compress != "zlib":
		sargv = append(sargv, "--compress-choice="+compress)
	}
	if !o.Sender() && o.skip_compress != "" {
		sargv = append(sargv, "--skip-compress="+o.skip_compress)
	}

	if o.Sender() && o.DoFsync() {
		sargv = append(sargv, "--fsync")
	}

	if o.block_size != 0 {
		sargv = append(sargv, fmt.Sprintf("-B%d", o.block_size))
	}

	// if (max_delete && am_sender) {
	// 	if (asprintf(&arg, "--max-delete=%d", max_delete) < 0)
	// 		goto oom;
	// 	args[ac++] = arg;
	// }

	// if (batch_prefix) {
	// 	char *r_or_w = write_batch ? "write" : "read";
	// 	if (asprintf(&arg, "--%s-batch=%s", r_or_w, batch_prefix) < 0)
	// 		goto oom;
	// 	args[ac++] = arg;
	// }

	if o.io_timeout != 0 {
		sargv = append(sargv, fmt.Sprintf("--timeout=%d", o.io_timeout))
	}

	// if (bwlimit) {
	// 	if (asprintf(&arg, "--bwlimit=%d", bwlimit) < 0)
	// 		goto oom;
	// 	args[ac++] = arg;
	// }

	// if (backup_dir) {
	// 	args[ac++] = "--backup-dir";
	// 	args[ac++] = backup_dir;
	// }

	// /* Only send --suffix if it specifies a non-default value. */
	// if (strcmp(backup_suffix, backup_dir ? "" : BACKUP_SUFFIX) != 0) {
	// 	/* We use the following syntax to avoid weirdness with '~'. */
	// 	if (asprintf(&arg, "--suffix=%s", backup_suffix) < 0)
	// 		goto oom;
	// 	args[ac++] = arg;
	// }

	// When a receiving server deletes (options.c:server_options).
	switch {
	case !o.Sender():
		if o.delete_mode != 0 && o.delete_excluded == 0 {
			sargv = append(sargv, "--delete")
		}
	case o.delete_before != 0:
		sargv = append(sargv, "--delete-before")
	case o.delete_during == 2:
		sargv = append(sargv, "--delete-delay")
	case o.delete_during != 0:
		sargv = append(sargv, "--delete-during")
	case o.delete_after != 0:
		sargv = append(sargv, "--delete-after")
	case o.delete_mode != 0 && o.delete_excluded == 0:
		sargv = append(sargv, "--delete")
	}
	if o.delete_excluded != 0 {
		sargv = append(sargv, "--delete-excluded")
	}

	// if (size_only)
	// 	args[ac++] = "--size-only";

	if o.modify_window_set {
		sargv = append(sargv, fmt.Sprintf("--modify-window=%d", o.modify_window))
	}

	if o.inplace != 0 {
		sargv = append(sargv, "--inplace")
	}

	if o.keep_partial != 0 {
		sargv = append(sargv, "--partial")
	}

	if o.partial_dir != "" {
		sargv = append(sargv, "--partial-dir="+o.partial_dir)
	}

	switch o.append_mode {
	case 1:
		sargv = append(sargv, "--append")
	case 2:
		sargv = append(sargv, "--append", "--append")
	}

	// if (force_delete)
	// 	args[ac++] = "--force";

	// if (delete_after)
	// 	args[ac++] = "--delete-after";

	// if (ignore_errors)
	// 	args[ac++] = "--ignore-errors";

	// What a receiving server decides by (options.c:server_options).
	if o.Sender() {
		if n := o.MaxDelete(); n > 0 {
			sargv = append(sargv, fmt.Sprintf("--max-delete=%d", n))
		} else if n == 0 {
			sargv = append(sargv, "--max-delete=-1")
		}
		if o.Force() {
			sargv = append(sargv, "--force")
		}
		if o.min_size >= 0 {
			sargv = append(sargv, "--min-size="+o.min_size_arg)
		}
		if o.max_size >= 0 {
			sargv = append(sargv, "--max-size="+o.max_size_arg)
		}
		if o.SizeOnly() {
			sargv = append(sargv, "--size-only")
		}
		if o.IgnoreExisting() {
			sargv = append(sargv, "--ignore-existing")
		}
		if o.IgnoreNonExisting() {
			sargv = append(sargv, "--existing")
		}
	}
	// Only a receiving server needs the alternate folders
	// (options.c:server_options).
	if o.Sender() {
		for _, dir := range o.basis_dirs {
			sargv = append(sargv, AltDestOption(o.alt_dest_type), dir)
		}
	}
	if o.remove_source_files == 1 {
		sargv = append(sargv, "--remove-source-files")
	} else if o.remove_source_files != 0 {
		sargv = append(sargv, "--remove-sent-files")
	}
	if o.IgnoreErrors() {
		sargv = append(sargv, "--ignore-errors")
	}
	if o.CopyUnsafeLinks() {
		sargv = append(sargv, "--copy-unsafe-links")
	}
	if o.SafeLinks() {
		sargv = append(sargv, "--safe-links")
	}

	if o.NumericIds() {
		sargv = append(sargv, "--numeric-ids")
	}

	// if (only_existing && am_sender)
	// 	args[ac++] = "--existing";

	// if (opt_ignore_existing && am_sender)
	// 	args[ac++] = "--ignore-existing";

	// if (tmpdir) {
	// 	args[ac++] = "--temp-dir";
	// 	args[ac++] = tmpdir;
	// }

	// if (compare_dest && am_sender) {
	// 	/* the server only needs this option if it is not the sender,
	// 	 *   and it may be an older version that doesn't know this
	// 	 *   option, so don't send it if client is the sender.
	// 	 */
	// 	args[ac++] = link_dest ? "--link-dest" : "--compare-dest";
	// 	args[ac++] = compare_dest;
	// }

	// if (files_from && (!am_sender || remote_filesfrom_file)) {
	// 	if (remote_filesfrom_file) {
	// 		args[ac++] = "--files-from";
	// 		args[ac++] = remote_filesfrom_file;
	// 		if (eol_nulls)
	// 			args[ac++] = "--from0";
	// 	} else {
	// 		args[ac++] = "--files-from=-";
	// 		args[ac++] = "--from0";
	// 	}
	// }

	// -M options go last, as they are (options.c:server_options).
	sargv = append(sargv, o.remote_options...)

	return sargv
}
