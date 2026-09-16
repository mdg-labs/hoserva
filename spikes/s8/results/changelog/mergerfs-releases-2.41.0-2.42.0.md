=== 2.41.0 ===
Despite the changelog not seeming too large there was a lot of internal reorganization and cleanup in preparation for these and future features. I tried to test the release to ensure no bugs or backwards compatibility issues (outside of some changes to some default values) but given the diversity and quantity of changes I could have missed something. Please submit a ticket if you find any problems: https://trapexit.github.io/mergerfs/latest/support/

## Major Changes and New Features

* [IO passthrough](https://trapexit.github.io/mergerfs/latest/config/passthrough/) - Near native read/write performance (on supported Linux versions, read docs for limitations.)
* [IO priority proxying](https://trapexit.github.io/mergerfs/latest/config/proxy-ioprio/) - When performing file IO use the same IO priority as the requesting app.
* create policy default changed from `epmfs` to `pfrd`.
* Full rework of documentation: https://trapexit.github.io/mergerfs/
* Updated default inode calculation to make it more stable across reboots.
* Improved secondary group cache which will expire over time and can be cleared as needed.
* `nofail` mount argument passthrough
* mergerfs will now set its `oom_score_adj` value to reduce likelihood of being killed by OOM killer.
* statx support (allows for btime to be queried)

## Other Changes

* Lots of internal improvements and restructuring.
* Addition of [fsck.mergerfs and mergerfs.collect-info](https://trapexit.github.io/mergerfs/latest/tooling/) tools.
* Enable auto mmap enablement when using cache.files=off (not relevant when io passthrough is used)
* Support for more than 256 page sized FUSE messages (not relevant when io passthrough is used)
* Ability to wait for [branches to mount](https://trapexit.github.io/mergerfs/latest/config/branches-mount-timeout/)
* Improved config / option management allowing FUSE and mount options to be placed in a config file.
* Changes which improve performance when using xattrs and cache.files!=off (not relevant when io passthrough is used)
* More logging and details to syslog (available via `journalctl -t mergerfs` where used)
* [A container image build for Docker, Podman, etc.](https://trapexit.github.io/mergerfs/latest/setup/installation/#rootless-container-runtimes)

## Full Git Log
* Suggest cache.files=auto-full rather than partial by @trapexit in https://github.com/trapexit/mergerfs/pull/1320
* Add support for 'direct-io-allow-mmap' if supported by kernel by @trapexit in https://github.com/trapexit/mergerfs/pull/1321
* Update README.md by @techie2000 in https://github.com/trapexit/mergerfs/pull/1342
* Add missing --relative flag to rsync in percent-full mover script by @grunthos503 in https://github.com/trapexit/mergerfs/pull/1346
* Add FAQ entry on 'move' and 'copy' by @trapexit in https://github.com/trapexit/mergerfs/pull/1377
* add mkdocs first draft by @oregonpillow in https://github.com/trapexit/mergerfs/pull/1382
* Rework mkdocs based documentation by @trapexit in https://github.com/trapexit/mergerfs/pull/1386
* Fix pip install in mkdocs workflow by @trapexit in https://github.com/trapexit/mergerfs/pull/1387
* Update README to point to docs, update project comparisons by @trapexit in https://github.com/trapexit/mergerfs/pull/1388
* Update funding and cirrus builds by @trapexit in https://github.com/trapexit/mergerfs/pull/1389
* Add tiered cache details to docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1390
* Move fuse.c and fuse_lowlevel.c to C++ by @trapexit in https://github.com/trapexit/mergerfs/pull/1391
* Send invalidate node request outside lock by @trapexit in https://github.com/trapexit/mergerfs/pull/1392
* fix edit uri by @oregonpillow in https://github.com/trapexit/mergerfs/pull/1395
* Improve FreeBSD compatibility by @trapexit in https://github.com/trapexit/mergerfs/pull/1398
* Readability changes to mkdocs installation by @oregonpillow in https://github.com/trapexit/mergerfs/pull/1399
* Doc and mover script updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1402
* Misc doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1407
* Add details about user namespacing and root squashing by @trapexit in https://github.com/trapexit/mergerfs/pull/1408
* Add details about logging to syslog by @trapexit in https://github.com/trapexit/mergerfs/pull/1409
* Rework questions on what settings and policies to use by @trapexit in https://github.com/trapexit/mergerfs/pull/1412
* Update quickstart.md by @theHenMan in https://github.com/trapexit/mergerfs/pull/1413
* Update terminology.md by @theHenMan in https://github.com/trapexit/mergerfs/pull/1414
* Update docs on out-of-band usage by @trapexit in https://github.com/trapexit/mergerfs/pull/1415
* Add FAQ section on common perceived problems by @trapexit in https://github.com/trapexit/mergerfs/pull/1416
* Add FAQ entry regarding inotify by @trapexit in https://github.com/trapexit/mergerfs/pull/1423
* Misc doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1425
* Misc doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1426
* Update docs on default create policy by @trapexit in https://github.com/trapexit/mergerfs/pull/1429
* Add links to options docs page to individual pages by @trapexit in https://github.com/trapexit/mergerfs/pull/1431
* Support Linux v6.13 FUSE max_page_limit by @trapexit in https://github.com/trapexit/mergerfs/pull/1433
* Change misc defaults by @trapexit in https://github.com/trapexit/mergerfs/pull/1434
* Replace usage of wyhash with rapidhash by @trapexit in https://github.com/trapexit/mergerfs/pull/1435
* Update docs regarding error handling by @trapexit in https://github.com/trapexit/mergerfs/pull/1438
* Add statx support by @trapexit in https://github.com/trapexit/mergerfs/pull/1439
* Add doc pages for minfreespace and moveonenospc + other misc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1441
* Change the "devino" inode calculation by @trapexit in https://github.com/trapexit/mergerfs/pull/1443
* Add FAQ entry regarding OS support by @trapexit in https://github.com/trapexit/mergerfs/pull/1444
* Misc doc updates + add logo by @trapexit in https://github.com/trapexit/mergerfs/pull/1445
* Improve mount waiting + misc doc improvements by @trapexit in https://github.com/trapexit/mergerfs/pull/1447
* Add config option to control default_permissions by @trapexit in https://github.com/trapexit/mergerfs/pull/1448
* Fixed a minor typo by @Solipsistmonkey in https://github.com/trapexit/mergerfs/pull/1450
* Rework thread pool for increased stability + config and doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1453
* Add podman release build tooling + misc build fixes by @trapexit in https://github.com/trapexit/mergerfs/pull/1455
* Update installation.md for SUSE by @gizak in https://github.com/trapexit/mergerfs/pull/1449
* Add versioning to docs using 'mike' by @trapexit in https://github.com/trapexit/mergerfs/pull/1456
* Add details to docs about FreeBSD limitations by @trapexit in https://github.com/trapexit/mergerfs/pull/1457
* Update README.md to fix link by @trapexit in https://github.com/trapexit/mergerfs/pull/1464
* Rework policies to return branches rather than path strings by @trapexit in https://github.com/trapexit/mergerfs/pull/1466
* Fix incorrect dyn cast by @trapexit in https://github.com/trapexit/mergerfs/pull/1467
* Add debugging of mutexes by @trapexit in https://github.com/trapexit/mergerfs/pull/1470
* Add support for FUSE passthrough by @trapexit in https://github.com/trapexit/mergerfs/pull/1472
* Misc cleanup by @trapexit in https://github.com/trapexit/mergerfs/pull/1473
* Spawn "mount" on branches when waiting for mounts enabled by @trapexit in https://github.com/trapexit/mergerfs/pull/1476
* Misc doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1477
* Add RHEL/CentOS 10 build support by @trapexit in https://github.com/trapexit/mergerfs/pull/1479
* Add fsck.mergerfs tool by @trapexit in https://github.com/trapexit/mergerfs/pull/1483
* Set default read thread count to 0 by @trapexit in https://github.com/trapexit/mergerfs/pull/1484
* Remove linux only getdents in favor of readdir for FreeBSD sake by @trapexit in https://github.com/trapexit/mergerfs/pull/1485
* Misc cleanup by @trapexit in https://github.com/trapexit/mergerfs/pull/1486
* Rework movefile and cow break to use copyfile by @trapexit in https://github.com/trapexit/mergerfs/pull/1488
* Misc documentation changes by @trapexit in https://github.com/trapexit/mergerfs/pull/1489
* Add mergerfs.collect-info by @trapexit in https://github.com/trapexit/mergerfs/pull/1491
* Update the secondary group cache by @trapexit in https://github.com/trapexit/mergerfs/pull/1492
* Ensure passthrough and keep_cache are mutually exclusive by @trapexit in https://github.com/trapexit/mergerfs/pull/1493
* Clear O_NOFOLLOW when opening a relative fd by @trapexit in https://github.com/trapexit/mergerfs/pull/1494
* Rework of runtime interface by @trapexit in https://github.com/trapexit/mergerfs/pull/1498
* Move everything to negative errno return types by @trapexit in https://github.com/trapexit/mergerfs/pull/1499
* Cleanup copy functions by @trapexit in https://github.com/trapexit/mergerfs/pull/1500
* Build improvements by @trapexit in https://github.com/trapexit/mergerfs/pull/1501
* More makefile tweaks, build log, smaller git clones by @trapexit in https://github.com/trapexit/mergerfs/pull/1503
* Add setting of oom_score_adj by @trapexit in https://github.com/trapexit/mergerfs/pull/1504
* Misc documentation updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1505
* Add Debian 13 build targets by @trapexit in https://github.com/trapexit/mergerfs/pull/1506
* Rework calculation of branch for release make targets by @trapexit in https://github.com/trapexit/mergerfs/pull/1507
* Fix constructing string from attrval setxattr config by @trapexit in https://github.com/trapexit/mergerfs/pull/1508
* Allow empty lines in config ini files by @Max-F-Helm in https://github.com/trapexit/mergerfs/pull/1509
* Updates to docs, add nonraid, more intro to fs details by @trapexit in https://github.com/trapexit/mergerfs/pull/1512
* Misc changes by @trapexit in https://github.com/trapexit/mergerfs/pull/1513
* Fix string manipulation functions after refactor by @trapexit in https://github.com/trapexit/mergerfs/pull/1514
* Update readme to match index of mkdocs by @trapexit in https://github.com/trapexit/mergerfs/pull/1515
* More build process updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1516
* Misc updates of docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1518
* Add Ubuntu 25.04 builds by @trapexit in https://github.com/trapexit/mergerfs/pull/1519
* Revert removal of readdir init setup, simplify cfg usage by @trapexit in https://github.com/trapexit/mergerfs/pull/1520
* Use std::filesystem::path for fusepath by @trapexit in https://github.com/trapexit/mergerfs/pull/1522
* idmap mount support by @trapexit in https://github.com/trapexit/mergerfs/pull/1523
* Misc docs updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1524
* Fix broken doc links by @trapexit in https://github.com/trapexit/mergerfs/pull/1525
* Update moveonenospc.md by @pjv in https://github.com/trapexit/mergerfs/pull/1527
* Add script to generate docs from scratch and push to github by @trapexit in https://github.com/trapexit/mergerfs/pull/1530
* Misc doc updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1531
* Fix typo in visualization and add new vid to docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1532
* Add getdents based readdir functions for Linux by @trapexit in https://github.com/trapexit/mergerfs/pull/1533
* Add ability to proxy ioprio of client apps by @trapexit in https://github.com/trapexit/mergerfs/pull/1534
* Remove need for and use of ioprio header by @trapexit in https://github.com/trapexit/mergerfs/pull/1535
* Misc fixes, mostly for FreeBSD by @trapexit in https://github.com/trapexit/mergerfs/pull/1536
* Misc build fixes by @trapexit in https://github.com/trapexit/mergerfs/pull/1537
* Fix dirent64::namelen calculation by @trapexit in https://github.com/trapexit/mergerfs/pull/1538
* Fix rename path generation by @trapexit in https://github.com/trapexit/mergerfs/pull/1540
* Make allow-idmap optional (default: false) by @trapexit in https://github.com/trapexit/mergerfs/pull/1541
* Allow setting of passthrough max-stack-depth by @trapexit in https://github.com/trapexit/mergerfs/pull/1542
* Rework how fuse request context is handled by @trapexit in https://github.com/trapexit/mergerfs/pull/1543
* Misc docs updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1544
* Rework config, centralize fuse config by @trapexit in https://github.com/trapexit/mergerfs/pull/1547
* Misc docs updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1549
* Fix lack of include for fmt by @trapexit in https://github.com/trapexit/mergerfs/pull/1550
* Config options docs updates by @trapexit in https://github.com/trapexit/mergerfs/pull/1551
* Update fuse-msg-size docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1552
* Fix variable names of fuse-msg-size and posix-acl by @trapexit in https://github.com/trapexit/mergerfs/pull/1553
* Misc updates to arg parsing by @trapexit in https://github.com/trapexit/mergerfs/pull/1554
* Fix manpage links for 2.41.0-rc3 by @satmandu in https://github.com/trapexit/mergerfs/pull/1555
* Add more debugging options to makefiles and cleanup by @trapexit in https://github.com/trapexit/mergerfs/pull/1556
* Mention how mmap error when direct io is enabled by @trapexit in https://github.com/trapexit/mergerfs/pull/1557
* Further tweaks to config parsing and error reporting by @trapexit in https://github.com/trapexit/mergerfs/pull/1559
* Improve mergerfs intro in docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1563
* Ensure backward compatibility with remember & noforget by @trapexit in https://github.com/trapexit/mergerfs/pull/1566
* Add ability to passthrough 'nofail' mount option by @trapexit in https://github.com/trapexit/mergerfs/pull/1567
* Add container image building by @trapexit in https://github.com/trapexit/mergerfs/pull/1568
* Add container image details by @trapexit in https://github.com/trapexit/mergerfs/pull/1569

## New Contributors
* @techie2000 made their first contribution in https://github.com/trapexit/mergerfs/pull/1342
* @oregonpillow made their first contribution in https://github.com/trapexit/mergerfs/pull/1382
* @theHenMan made their first contribution in https://github.com/trapexit/mergerfs/pull/1413
* @Solipsistmonkey made their first contribution in https://github.com/trapexit/mergerfs/pull/1450
* @gizak made their first contribution in https://github.com/trapexit/mergerfs/pull/1449
* @Max-F-Helm made their first contribution in https://github.com/trapexit/mergerfs/pull/1509
* @pjv made their first contribution in https://github.com/trapexit/mergerfs/pull/1527
* @satmandu made their first contribution in https://github.com/trapexit/mergerfs/pull/1555

**Full Changelog**: https://github.com/trapexit/mergerfs/compare/2.40.2...2.41.0

=== 2.41.1 ===
## Change Summary

* Fix error in size calculation for listxattr requests. Impacted "xattr" tool which failed to respond properly to ERANGE errors.
* Fix a order of initialization bug that was causing crashes for some users.


## Full Git Log
* Add link to container image repo by @trapexit in https://github.com/trapexit/mergerfs/pull/1570
* Add lazy-umount-mountpoint page and mount -a FAQ by @trapexit in https://github.com/trapexit/mergerfs/pull/1573
* Fix size comparison for returning ERANGE in listxattr by @trapexit in https://github.com/trapexit/mergerfs/pull/1574
* Slight tweaks to config parsing error handling by @trapexit in https://github.com/trapexit/mergerfs/pull/1575
* Add Ubuntu 25.10 build by @trapexit in https://github.com/trapexit/mergerfs/pull/1577
* Add FAQ about lack of create policies about file size by @trapexit in https://github.com/trapexit/mergerfs/pull/1578
* Ensure cfg references do not also try to initialize the value by @trapexit in https://github.com/trapexit/mergerfs/pull/1580
* Fix some links in docs by @trapexit in https://github.com/trapexit/mergerfs/pull/1582


**Full Changelog**: https://github.com/trapexit/mergerfs/compare/2.41.0...2.41.1

=== 2.42.0 ===
# mergerfs v2.42.0

## Donations / Sponsorship

If you find mergerfs useful please consider supporting its ongoing development.

https://github.com/trapexit/support

## New features

* `lup` (least used percentage) policy: selects the branch with the lowest used space percentage.
* `mount.mergerfs` now includes the `-n` flag to support mounting without updating /etc/mtab.

## Improvements

* Better lock management and behavior with open files. Reduction of contention.
* Custom filename dedup strategy improving directory reading performance
* Pre-calculate part of the file inode within readdir significantly reducing cost of overall calculation.
* Replace sched_yield with nanosleep for improved scheduling of contention situations.
* improved stat auto cache fingerprinting to reduce stale reads.

## Behavior changes

* credential handling reworked to be compatible with chroot and idmap. removed ability to disable `default_permissions` option as a result as mergerfs now requires the kernel to manage entitlements.
* broken mounts with ENOTCONN (common due to crashed FUSE instances) are automatically unmounted if possible on start instead of hard erroring. (mostly useful while developing).
* `fusermount3` will be used if available and other options missing.
* `remember-nodes` option is deprecated and the feature removed. There is now only `never-forget-nodes` behavior. Any non-zero value in `remember-nodes` will enable `never-forget-nodes`. It was removed because time based really didn't save much memory and complicated the code. Doing this also reduced the size of each node helping offset increased node count.

## Bug fixes

* fix race condition between runtime changing of branches and certain requests.
* `rmdir` no longer returns success if any rmdir call returns ENOTEMPTY.
* Invalid policy names are properly rejected.
* statfs calculation could theoretically have overflowed with impossibly large branch values.
* Fixed `readlink` buffer size management. The size does not include a nul terminating value.
* Minor calculation bugs in `copy_file_range` and `futimens`.
* `moveonenospc` no longer crashes when all destination branches are rejected. Returns ENOSPC.
* Better / more proper error calculations in a number of functions.
* rare ioctl call crash fixed

## Hardening

* many "the kernel should never do this bug **just** in case" enhancements

## Full Changelog

https://github.com/trapexit/mergerfs/compare/2.41.1...2.42.0-rc2



