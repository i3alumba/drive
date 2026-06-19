import { useState, type DragEvent } from "react";
import {
  Box,
  Button,
  Card,
  CardActionArea,
  CardContent,
  CardMedia,
  Chip,
  IconButton,
  Tooltip,
  Typography,
} from "@mui/material";
import DeleteIcon from "@mui/icons-material/Delete";
import type { DriveObject } from "../types";
import {
  driveDragType,
  getKind,
  iconForKind,
  readDraggedItem,
} from "../utils/fileUtils";

type Props = {
  item: DriveObject;
  canEdit: boolean;
  canShare: boolean;
  viewUrl: string;
  onOpen: () => void;
  onDelete: () => void;
  onMove: (item: DriveObject, destinationDir: string) => Promise<void>;
  onShare: () => void;
};

export function FileCard({
  item,
  canEdit,
  canShare,
  viewUrl,
  onOpen,
  onDelete,
  onMove,
  onShare,
}: Props) {
  const [isDropTarget, setIsDropTarget] = useState(false);
  const kind = getKind(item.name, item.isDir);

  function handleDragStart(event: DragEvent) {
    event.dataTransfer.effectAllowed = "move";
    event.dataTransfer.setData(driveDragType, JSON.stringify(item));
  }

  function handleDragOver(event: DragEvent) {
    if (
      !canEdit ||
      !item.isDir ||
      !event.dataTransfer.types.includes(driveDragType)
    ) {
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    setIsDropTarget(true);
  }

  async function handleDrop(event: DragEvent) {
    if (!canEdit || !item.isDir) return;
    event.preventDefault();
    event.stopPropagation();
    setIsDropTarget(false);
    const dragged = readDraggedItem(event);
    if (dragged) await onMove(dragged, item.path);
  }

  return (
    <Card
      variant="outlined"
      draggable={canEdit}
      onDragStart={canEdit ? handleDragStart : undefined}
      onDragOver={handleDragOver}
      onDrop={handleDrop}
      onDragLeave={() => setIsDropTarget(false)}
      sx={{
        borderColor: isDropTarget ? "primary.main" : undefined,
        borderWidth: isDropTarget ? 2 : 1,
      }}
    >
      <CardActionArea onClick={onOpen}>
        <Box
          sx={{
            height: 128,
            bgcolor: isDropTarget ? "primary.50" : "grey.100",
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            overflow: "hidden",
          }}
        >
          {kind === "image" && (
            <CardMedia
              component="img"
              image={viewUrl}
              alt={item.name}
              sx={{ width: "100%", height: "100%", objectFit: "cover" }}
            />
          )}
          {kind === "video" && (
            <CardMedia
              component="video"
              src={viewUrl}
              muted
              preload="metadata"
              sx={{ width: "100%", height: "100%", objectFit: "cover" }}
            />
          )}
          {kind !== "image" && kind !== "video" && iconForKind(kind, 56)}
        </Box>
        <CardContent sx={{ pb: 1 }}>
          <Tooltip title={item.name}>
            <Typography noWrap>{item.name}</Typography>
          </Tooltip>
          <Typography variant="caption" color="text.secondary">
            {item.isDir ? "Directory" : `${Math.round(item.size / 1024)} KB`}
          </Typography>
        </CardContent>
      </CardActionArea>
      <Box
        sx={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          px: 1,
          pb: 1,
        }}
      >
        <Chip size="small" label={item.isDir ? "folder" : kind} />
        <Box>
          {canShare && (
            <Button
              size="small"
              onClick={(event) => {
                event.stopPropagation();
                onShare();
              }}
            >
              Share
            </Button>
          )}
          {canEdit && (
            <IconButton
              size="small"
              onClick={(event) => {
                event.stopPropagation();
                onDelete();
              }}
            >
              <DeleteIcon fontSize="small" />
            </IconButton>
          )}
        </Box>
      </Box>
    </Card>
  );
}
