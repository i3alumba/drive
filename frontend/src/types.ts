export type DriveObject = {
  name: string;
  path: string;
  size: number;
  lastModified: string;
  isDir: boolean;
};

export type TorrentJob = {
  id: string;
  name: string;
  targetDir: string;
  status: string;
  progress: number;
  error?: string;
};

export type Space = {
  id: string;
  name: string;
  permission: "read" | "edit";
  shared?: boolean;
};

export type FileKind =
  | "image"
  | "video"
  | "audio"
  | "text"
  | "pdf"
  | "office"
  | "other";
