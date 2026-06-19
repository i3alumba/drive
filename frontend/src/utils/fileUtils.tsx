import AudioFileIcon from "@mui/icons-material/AudioFile";
import DescriptionIcon from "@mui/icons-material/Description";
import FolderIcon from "@mui/icons-material/Folder";
import ImageIcon from "@mui/icons-material/Image";
import InsertDriveFileIcon from "@mui/icons-material/InsertDriveFile";
import MovieIcon from "@mui/icons-material/Movie";
import type { DragEvent } from "react";
import type { DriveObject, FileKind } from "../types";

const textExtensions = new Set([
  "txt",
  "md",
  "json",
  "csv",
  "log",
  "xml",
  "yaml",
  "yml",
  "go",
  "ts",
  "tsx",
  "js",
  "jsx",
  "py",
  "rs",
  "java",
  "c",
  "cpp",
  "h",
  "css",
  "html",
]);
const officeExtensions = new Set([
  "doc",
  "docx",
  "odt",
  "ods",
  "odp",
  "ppt",
  "pptx",
  "xls",
  "xlsx",
  "rtf",
]);

export const driveDragType = "application/x-drive-item";

export function getKind(name: string, isDir: boolean): FileKind | "folder" {
  if (isDir) return "folder";
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  if (["jpg", "jpeg", "png", "gif", "webp", "bmp", "svg"].includes(ext)) {
    return "image";
  }
  if (["mp4", "webm", "ogg", "ogv", "mov", "m4v"].includes(ext)) {
    return "video";
  }
  if (["mp3", "wav", "flac", "m4a", "aac", "oga", "opus"].includes(ext)) {
    return "audio";
  }
  if (ext === "pdf") return "pdf";
  if (textExtensions.has(ext)) return "text";
  if (officeExtensions.has(ext)) return "office";
  return "other";
}

export function iconForKind(kind: FileKind | "folder", size: number) {
  const sx = { fontSize: size, color: "text.secondary" };
  if (kind === "folder")
    return <FolderIcon sx={{ ...sx, color: "warning.main" }} />;
  if (kind === "image") return <ImageIcon sx={sx} />;
  if (kind === "video") return <MovieIcon sx={sx} />;
  if (kind === "audio") return <AudioFileIcon sx={sx} />;
  if (kind === "text" || kind === "pdf" || kind === "office") {
    return <DescriptionIcon sx={sx} />;
  }
  return <InsertDriveFileIcon sx={sx} />;
}

export function readDraggedItem(event: DragEvent): DriveObject | null {
  const raw = event.dataTransfer.getData(driveDragType);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as DriveObject;
  } catch {
    return null;
  }
}

export function baseName(path: string) {
  return path.split("/").filter(Boolean).pop() ?? path;
}

export function joinPath(dir: string, name: string) {
  return [dir.replace(/\/$/, ""), name].filter(Boolean).join("/");
}
