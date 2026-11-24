/*
 * Fork-only: the process .github/kamin/criu-link-remap-test.sh dumps.
 *
 * Usage: criu-link-remap-holder /<semaphore name> <ready file>
 *
 * Creates the POSIX named semaphore with sem_open(3), keeps it mapped and its
 * name linked, writes its PID to <ready file> and waits to be dumped. glibc
 * creates the semaphore in /dev/shm under a temporary name, maps that file,
 * links it to sem.<semaphore name> and removes the temporary name, so the
 * process maps a removed file that still has a link: CRIU saves it as a link
 * remap, a hard link named link_remap.<id> in /dev/shm. Python's
 * multiprocessing locks, for example, are such semaphores.
 */
#define _GNU_SOURCE
#include <fcntl.h>
#include <semaphore.h>
#include <stdio.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	char tmp[4096];
	sem_t *sem;
	FILE *f;

	if (argc != 3) {
		fprintf(stderr, "usage: %s /<semaphore name> <ready file>\n", argv[0]);
		return 2;
	}
	/* Nothing but the semaphore and /dev/null on 0, 1 and 2 for CRIU to save. */
	if (close_range(3, ~0U, 0) || chdir("/") || setsid() < 0) {
		perror("setup");
		return 1;
	}
	sem = sem_open(argv[1], O_CREAT | O_EXCL, 0600, 1);
	if (sem == SEM_FAILED) {
		perror("sem_open");
		return 1;
	}
	snprintf(tmp, sizeof(tmp), "%s.tmp", argv[2]);
	f = fopen(tmp, "w");
	if (!f || fprintf(f, "%d\n", getpid()) < 0 || fclose(f) || rename(tmp, argv[2])) {
		perror("ready file");
		return 1;
	}
	for (;;)
		pause();
}
