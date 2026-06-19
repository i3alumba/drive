import {
  Box,
  Button,
  Dialog,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
} from "@mui/material";
import type { DriveObject } from "../types";

type Props = {
  item: DriveObject | null;
  target: string;
  permission: "read" | "edit";
  onTargetChange: (value: string) => void;
  onPermissionChange: (value: "read" | "edit") => void;
  onClose: () => void;
  onShare: () => void;
};

export function ShareDialog({
  item,
  target,
  permission,
  onTargetChange,
  onPermissionChange,
  onClose,
  onShare,
}: Props) {
  return (
    <Dialog open={Boolean(item)} onClose={onClose} maxWidth="xs" fullWidth>
      <DialogTitle>Share {item?.name}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          <TextField
            label="Target username"
            value={target}
            onChange={(event) => onTargetChange(event.target.value)}
            helperText="Use the username from the main auth service."
          />
          <Box
            component="select"
            value={permission}
            onChange={(event) =>
              onPermissionChange(event.target.value as "read" | "edit")
            }
            sx={{ p: 1, borderRadius: 1, borderColor: "divider" }}
          >
            <option value="read">Read-only</option>
            <option value="edit">Edit</option>
          </Box>
          <Stack
            direction="row"
            spacing={1}
            sx={{ justifyContent: "flex-end" }}
          >
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="contained" onClick={onShare}>
              Share
            </Button>
          </Stack>
        </Stack>
      </DialogContent>
    </Dialog>
  );
}
