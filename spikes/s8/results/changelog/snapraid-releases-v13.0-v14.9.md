=== v13.0 ===
 * Added new thermal protection configuration options:
    - temp_limit TEMPERATURE_CELSIUS
      Sets the maximum allowed disk temperature. When any disk exceeds this
      limit, SnapRAID stops all operations and spins down the disks to prevent
      overheating.
    - temp_sleep TIME_IN_MINUTES
      Defines how long the disks remain in standby after a temperature limit
      event. After this time, operations are resumed. Defaults to 5 minutes.
 * Added a new "probe" command that shows the spinning status of all disks.
 * Added a new -s, --spin-down-on-error option that spins down all disks when
   a command ends with an error.
 * Added a new -A, --stats option for an extensive view of the process.
 * Fixed handling of command-line arguments containing UTF-8 characters on
   Windows, ensuring proper processing outside the Windows code page.
 * Removed the SMART attribute 193 "Load Cycle Count" from the failure
   probability computation, as its effectiveness in predicting failures is too
   dependent on the hard disk vendor.
 * Added a new "smartignore" configuration option to ignore specific SMART
   attributes.
 * Supported UUID in macOS [Nasado]
 * Windows binaries built with gcc 11.5.0 using the MXE cross compiler at
   commit 8c4378fa2b55bc28515b23e96e05d03e671d9b90 with targets
   i686-w64-mingw32.static and x86_64-w64-mingw32.static and optimization -O2.


=== v14.0 ===
 * More log tags for all commands in preparation for the upcoming SnapRAID
   Daemon. Pre-release will be available at https://github.com/amadvance/snapraid-daemon.
   Note that this is the minimal SnapRAID version required to use the daemon.
 * New 'extra' config option to define additional disks to monitor with the
   'probe' and 'smart' commands. This replaces the previous autodetection.
 * Introduced the 'relocated' file state to unify the previous 'copied/removed'
   logic. This new status specifically identifies files moved to a different
   path or disk with a new inode where the original has disappeared.
 * Added support for local exclusion rules via .snapraidignore files placed
   directly within the array directory tree. This allows for granular,
   directory-level control over which files and folders are excluded from
   parity, mirroring the workflow of .gitignore.
 * Added support for the ** globbing character to allow recursive pattern
   matching across multiple directory levels.
 * Fixed a crash on macOS when filesystem doesn't report UUID
 * Added a new locate command to map physical file offsets within the
   parity volume. This feature facilitates diagnostic analysis of the parity
   distribution. It supports the -t, --tail option to filter files located
   specifically at the end of the parity file [Ralf1108].
 * Added new option `-W, --force-realloc-tail SIZE` for the `sync` command.
   This option works like `--force-realloc` but applies only to the
   specified tail portion (last SIZE bytes) of the parity file.
   Its main purpose and result is to shrink the size of the parity file by
   reclaiming unused space ("holes") that may exist in the parity due to
   previous fragmentation, allowing to compact the parity toward the beginning
   [Ralf1108].
 * The -p / --plan option now accepts percentage values with decimal points
   (e.g. -p 1.5, -p 0.2)
 * Added detection of the smartctl executable. Improves reliability when running
   under sudo or in environments where /sbin and /usr/sbin are not in PATH.
 * Avoided saving the content file when it was not strictly necessary.
 * Added wear level percentage to the SMART report.
 * Added documentation of the tags used in the log file in the new
   snapraid_log manpage.
 * Added manual translations in multiple languages.
 * Added new undocumented options that affect the -T, --speed-test:
   --speed-test-period MS, specifies how many milliseconds to test each single
     feature in the speed test. Default 1000.
   --speed-test-disks-number DISKS, specifies how many disks to use in the
     speed test. Default 8.
   --speed-test-blocks-size KB, specifies the size of each block in kibibytes
     (1024) used in the speed test. Default 256.

=== v14.1 ===
 * Fixed include/exclude specification for directories. This is a regression
   bug in version 14.0. It is highly recommended to update.
   If you are using version 14.0 with include/exclude directives, simply run
   a new sync with this version to automatically resolve any potential issues.
 * Fixed a build issue in Alpine distribution.
 * Fixed abort condition if the Windows volume name cannot be converted to
   UTF-8.

=== v14.2 ===
 * Added a workaround for filesystems with a broken implementation of
   posix_fadvise() that returns unexpected error codes.
   This issue was specifically reported with the F2FS filesystem.
   This change is primarily intended for package maintainers who need to run
   "make check" on build servers that may use such filesystems.
   No update is required if you have not encountered an error message about
   failure to advise files.
 * Log the modification time of the content file in probe and other commands.
   This log entry is used by SnapRAID Daemon 1.6.


=== v14.3 ===
 * On ARM64 (AArch64), replaced the inline assembly implementation of
   128-bit multiplication with a `__uint128_t`-based version.
   This avoids potential miscompilations of the experimental MuseAir
   hash observed with aggressive compiler optimizations (e.g. -O2/-O3)
   on some toolchains (notably Apple Silicon), while still generating
   optimal code on modern compilers.
 * Fixed the speed computation of the undocumented -T option for the
   recovery functions. The reported values are now comparable to those
   of the generation functions. Note that the real speed is unchanged
   from the previous version, only the way it is measured has changed.



=== v14.4 ===
 * Updated --gui-threshold-* logic to trigger on values equal to or greater
   than the threshold (previously only "greater than"). This enables setting
   the threshold to 1 to trigger it for a single case.
   This change is required by SnapRAID Daemon 1.8.


=== v14.5 ===
 * Support importing data from smartctl even when the disk reports
   incomplete information for attributes (e.g. missing norm, worst, or
   threshold values).
 * Log errors while SnapRAID scans the list of all files on a disk, so
   that the SnapRAID Daemon becomes aware of any I/O errors.
 * Fixed importing of the disk serial number from the SCSI Device
   Identification VPD page (also known as VPD Page 83h).
 * Various security improvements and memory leak fixes.


=== v14.6 ===
 * Fix stack overflow in deep directory trees on platforms with a small
   stack like Alpine.
 * Log errors as "fatal" only if the execution is stopped early.
   This ensures SnapRAID Daemon correctly identifies the alert level of 
   each message (e.g., treating non-terminating issues as warnings).



=== v14.7 ===
 * Fix the 'touch' command on Windows for files with the read-only
   attribute. The read-only attribute is temporarily removed and 
   then restored. No need to update if you are not on Windows.


=== v14.8 ===
 * Avoid spinning up standby disks on Windows in some machines during
   the 'probe' and other device-monitoring commands. Direct system
   queries for disk properties, size, and geometry are now performed
   using zero-access handles.
 * Fixed the smartctl type detection retry during probe on Windows to
   properly preserve the standby check option, preventing standby disks
   from spinning up if a retry is needed.
 * Save the content file before aborting 'sync' due to safety thresholds
   if files were touched by '--gui-touch-before', preventing timestamp
   desynchronization and false diff reports on subsequent runs.


=== v14.9 ===
 * Fix a regression when both the '--gui-touch-before' and
   '--gui-threshold' options are selected at the same time, which may
   prevent the thresholds from working correctly. Now, the content file
   is unconditionally saved before scanning for differences if any touch
   is performed.


