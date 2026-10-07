// Package overlayfs presents a project directory merged with overlay source
// directories as one FUSE filesystem: directories merge at every depth,
// colliding text files are joined, and writes land in the layer that owns them.
package overlayfs
