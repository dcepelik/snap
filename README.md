# Snap

Snap is a snapshot manager for Btrfs, similar to Snapper but much simpler. I've
been using for several years now without any trouble, but I encourage you to
read the code yourself, since it's just some 500 lines of Go.

## Usage

### Creating a Snap profile

First, configure a profile, such as:

    {
      "Profiles": {
        "root": {
          "Buckets": [
            {
              "Interval": "2m",
              "Size": 60
            },
            {
              "Interval": "1h",
              "Size": 48
            },
            {
              "Interval": "1d",
              "Size": 14
            },
            {
              "Interval": "1w",
              "Size": 4
            }
          ],
          "Storage": "/snap/root",
          "Subvolume": "/"
        }
      }
    }

By default, Snap looks for configuration in _/etc/snap/config.json_. There's
a more complex example provided in _config.example.json_.

### Creating snapshots

To create a snapshot of your _/_ in _/snap/root_, run:

    ~% sudo snap -c root

Since Btrfs snapshots are cheap, you can take one every minute or two. You can
use systemd timers, cron or a runit service to supervise snapshot creation.

### Listing snapshots

To list all your snapshots, run:

    ~% snap -l root
    1         6w 42h  ago    /snap/root/2023-09-23_18:22:12
    2         5w 16h  ago    /snap/root/2023-10-01_20:07:35
    3        27d 23h  ago    /snap/root/2023-10-09_13:27:46
    4        20d  5h  ago    /snap/root/2023-10-17_06:36:32
    [...]
    127          19s  ago    /snap/root/2023-11-06_12:34:08

### Listing paths across all snapshots

To list instances of a path across all snapshots:

    ~/snap% (master) snap -L main.go root
     -rw-r--r--  13090   5mo10d  /snap/root/2023-11-06_12:23:43/snapshot/home/d/code/dotfiles/sw/snap/main.go
     -rw-r--r--  13177  15m 11s  /snap/root/2023-11-06_12:25:43/snapshot/home/d/code/dotfiles/sw/snap/main.go
     -rw-r--r--  13090  13m 23s  /snap/root/2023-11-06_12:39:45/snapshot/home/d/code/dotfiles/sw/snap/main.go

This is useful when you mess up and want to recover a particular version of a file.
Usually you can tell the right one by file mode, file length or last modified date.
You can then easily copy-paste the path to the file from the listing and just cp(1)
it out. There's also a helper script called _snapback_ which can recover a file
from its most recent snapshot.

The path may also be a directory.

### Deleting old snapshots

To retain only those snapshots which fit into your configured buckets, run:

    ~% sudo snap -X --dry-run root
    exec.Command(subvolume, delete, "/snap/root/2023-11-06_12:36:50/snapshot")

The buckets work as follows:

- When the contents of the snapshot store is loaded, each snapshot is inserted
  into the top bucket, from the oldest to the most recent one.

- When a snapshot is inserted into a full bucket, the oldest snapshot from that
  bucket falls through to the next bucket.

- Some snapshots fall out of the last bucket, these are subject to removal.

Use systemd timers, cron or a runit service to clean up snapshots regularly.
I run cleanup approximately every 30 minutes.

### Synchronizing snapshots

When you store snapshots on the same device as your data, you are only
protected against a narrow class of mishaps, namely accidental file
deletion/overwrite. There is a ton of value in doing this (it saved my day on
many occasions), but it doesn't replace a proper backup routine.

Snapshots can, however, be used as a foundation for proper backups: by sending
them to another device. An additional profile is needed for that:

    {
      /* ... */

      "root-backup": {
        "Backup": "root",
        "Buckets": [
          {
            "Interval": "1h",
            "Size": 48
          },
          {
            "Interval": "1d",
            "Size": 60
          }
        ],
        "Storage": "/mnt/backup/snap/root"
      }
    }

I have a USB-C enclosure with a 2 TB NVMe SSD in it which I send my snapshots
to. Since my primary concern is loss/theft of my laptop and hardware failure of
my laptop's stock SSD, I keep the drive hidden at home (hidden well enough so
that a random burglar doesn't find it by chance) and I never keep it where my
laptop is.

The drive is encrypted and I have a little helper script called _backup_ which
unlocks the external drive, mounts the file system at _/mnt/backup/snap/root_,
sends new snapshots to it and prunes the external store. (As you can see above, the
backup profile has its own retention policy.)

Crucially, since I backup every day, there's always a recent snapshot which is
present on both my laptop and the NVMe drive (usually the previous daily
snapshot, or last week's snapshot if I skip many backups, etc.) and so it's
enough to send the difference between each new snapshots and the most recent
snapshot which is present on both drives. Btrfs supports this out of the box,
see btrfs send -p.

I hate when backups take long. This way, I only send small deltas, which takes
less than a minute when done daily. The NVMe enclosure is USB 3.2 20 Gbps (also
known as USB 3.2 gen 2x2 at one point in time), and the drive is Samsung 970
EVO Plus.

From time to time, I check the Btrfs on the external drive.

To backups snapshots:

    ~% sudo snap -b root-backup

I encourage you to read the simple backup script to get the full picture.

### Q: Why not send the backups to S3?

My reasons are mostly subjective:

- You just can't beat the speed.

- I'm not happy with my personal data laying around in 3rd-party storage, even
  if encrypted. All current forms of encryption will be vulnerable some day, and
  there's no forward secrecy with backups.

- A high-quality NVMe drive with an external enclosure and a USB 4 cable is a
  bit of an investment, but in the long run it's still much cheaper than S3.

- Since I already have snapshots, building backups around them seems like the
  right choice. If I had a better uplink and didn't worry about privacy, I would
  just run [BS3] over [BUSE] and sync there.

## Contributing and reporting bugs

Contributions and bug reports are welcome!

## License

MIT

## Acknowledgements

I first heard about this approach from [Vojtěch Aschenbrenner][va].

Many other tools exist which do something similar, or possibly even better.
For example, there's [btrbk]. I rolled my own because I could get away with
something much simpler that I actually understand. When it comes to backups,
that's a good thing!

[BS3]: https://github.com/asch/bs3
[BUSE]: https://github.com/asch/buse
[va]: https://asch.cz/
[btrbk]: https://github.com/digint/btrbk
