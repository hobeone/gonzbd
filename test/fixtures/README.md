# Test Fixtures

Pre-built archive and data fixtures for integration and post-processing
tests. These are committed to the repo so tests don't need the creation
tools (e.g. `rar`) at test time — only the extraction tools (`unrar`,
`7z`, `par2`) are required.

## Directories

### `nzb/`
Minimal NZB XML files for parser tests.

### `7z/`
A small `.7z` archive containing `sample.txt`.
`sample.txt.sha256` holds the expected SHA-256 of the extracted file.

### `par2/`
A `data.bin` file with its par2 verification and recovery set.
`data.bin.sha256` holds the expected SHA-256 of the intact file.
Used to test par2 verify and repair operations.

`par2/layout_b/` is a Layout B post: `release.rar` (RAR5, compressed) is the
only payload, and `feature.par2` + `feature.vol0+1.par2` protect
`feature.bin`, the file it extracts to, which is not delivered.
`release.7z` is the same content as a 7z archive.
`feature.bin.sha256` holds the SHA-256 of the extracted file. The archive is
compressed on purpose: a stored (`-m0`) archive carries the protected file's
bytes verbatim, and par2's block scan would find them inside it.
`damaged_b2.rar` is a stored RAR5 of the same `feature.bin` that records only
a BLAKE2sp digest (`-htb`), with four bytes of the member overwritten at
archive offset 20000. `go_rar` cannot check that digest, so it extracts the
damage without an error; the recovery block in `feature.vol0+1.par2` repairs
it.

`par2/layout_b_mixed/` holds two more par2 sets for mixed identification.
`withnfo.par2` + `withnfo.vol0+1.par2` protect `feature.bin` (the Layout B
payload above, not committed here) and the delivered sidecar `feature.nfo`.
`extras.par2` + `extras.vol0+1.par2` protect `extras.txt`, an ordinary
delivered file.

`par2/layout_a/` protects an archive. `Real.Name.par2` +
`Real.Name.vol0+2.par2` were created over `Real.Name.rar` (a RAR5 of the
same `feature.bin`), which is not committed. `d8f7a6.rar` is that archive
under an obfuscated name with four bytes overwritten at offset 600, inside
its first 16 KB, which the recovery volume repairs. `outer.rar` is a RAR
whose one member is `Real.Name.rar`.

### `split/`
A 3 KB file split into three 1 KB parts (`sample.001`, `.002`, `.003`).
`joined.bin.sha256` holds the expected SHA-256 of the reassembled file.
Used to test the file-join post-processing stage.

### `rar/`
Reserved for a pre-built RAR archive. Creating RAR files requires the
proprietary `rar` binary. If you have it, create a fixture with:

```bash
echo "RAR fixture content" > sample.txt
rar a sample.rar sample.txt
sha256sum sample.txt > sample.txt.sha256
```

### `obfuscated/`
A file with a random hex name (`a8f3b2c1d4e5f6a7.bin`) to test the
deobfuscation pipeline. `expected.sha256` holds its SHA-256.

## Regenerating Fixtures

If you need to regenerate (e.g. after changing test expectations):

```bash
# 7z (requires 7z)
echo "content" > sample.txt && 7z a sample.7z sample.txt

# par2 (requires par2)
par2 create -r10 -n1 data.par2 data.bin

# par2/layout_b (requires rar and par2)
seq 1 7000 | awk '{print "line " $1 " of the Layout B fixture payload"}' | head -c 40000 > feature.bin
rar a -m5 -ma5 release.rar feature.bin
par2 create -s4000 -r10 -n1 feature.par2 feature.bin
7z a -mx=9 release.7z feature.bin
sha256sum feature.bin > feature.bin.sha256
rar a -m0 -ma5 -htb damaged_b2.rar feature.bin
printf '\xff\xff\xff\xff' | dd of=damaged_b2.rar bs=1 seek=20000 conv=notrunc

# par2/layout_b_mixed (requires par2), from the same feature.bin
printf 'Feature release notes.\nSource: fixture.\n' > feature.nfo
par2 create -s4000 -r10 -n1 withnfo.par2 feature.bin feature.nfo
printf 'extras payload, protected by its own par2 set\n' > extras.txt
par2 create -s64 -r10 -n1 extras.par2 extras.txt

# par2/layout_a (requires rar and par2), from the same feature.bin
rar a -m5 -ma5 Real.Name.rar feature.bin
par2 create -s256 -r30 -n1 Real.Name.par2 Real.Name.rar
cp Real.Name.rar d8f7a6.rar
printf '\xff\xff\xff\xff' | dd of=d8f7a6.rar bs=1 seek=600 conv=notrunc
rar a -m5 -ma5 outer.rar Real.Name.rar
rm feature.bin Real.Name.rar

# split (coreutils)
split -b 1024 -d -a 3 source.bin sample.
# then rename .000→.001, .001→.002, .002→.003
```
