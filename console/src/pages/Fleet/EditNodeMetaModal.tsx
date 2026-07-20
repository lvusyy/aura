import { useEffect, useState } from "react";
import { Form, Input, Modal, Typography, message } from "antd";
import { consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type { UpdateNodeMetaResponse } from "../../gen/aura/v1/console_pb";

// 统一错误信息提取（ConnectError 继承 Error，.message 即含 code 前缀）。
function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

interface MetaForm {
  label: string;
  location: string;
  project: string;
}

// EditNodeMetaModal：编辑节点 label/location（nodes 表持久，TASK-005 UpdateNodeMeta RPC）。
// name 由节点自报不经此编辑（仅 label/location 为用户可编维度）。成功后控制面广播 FleetEvent，
// useFleetStream 增量 upsert 自动刷新列表——本组件只负责发起更新与关闭，不手动重拉。
export function EditNodeMetaModal({
  node,
  open,
  onClose,
}: {
  node: NodeInfo | null;
  open: boolean;
  onClose: () => void;
}) {
  const [form] = Form.useForm<MetaForm>();
  const [saving, setSaving] = useState(false);

  // 打开或切换目标节点时以当前值回填表单（Modal 不销毁，需显式同步）。
  useEffect(() => {
    if (open && node) {
      form.setFieldsValue({ label: node.label, location: node.location, project: node.project });
    }
  }, [open, node, form]);

  const onSave = async () => {
    if (!node) {
      return;
    }
    let v: MetaForm;
    try {
      v = await form.validateFields();
    } catch {
      return; // 校验失败，AntD 已内联提示
    }
    setSaving(true);
    try {
      const res: UpdateNodeMetaResponse = await consoleClient.updateNodeMeta({
        nodeId: node.nodeId,
        label: v.label?.trim() ?? "",
        location: v.location?.trim() ?? "",
        // M15：project 为 optional（presence 语义），编辑面总是写入归属（含空=清除）。项目令牌改归属
        // 会被后端拒（PermissionDenied），错误经 message.error 呈现。
        project: v.project?.trim() ?? "",
      });
      if (!res.updated) {
        message.warning("节点不存在或未更新");
        return;
      }
      message.success("已更新节点信息，列表将实时刷新");
      onClose();
    } catch (e) {
      message.error(`更新失败：${errText(e)}`);
    } finally {
      setSaving(false);
    }
  };

  const displayName = node ? node.name || node.nodeId : "";

  return (
    <Modal
      open={open}
      title="编辑节点信息"
      okText="保存"
      cancelText="取消"
      confirmLoading={saving}
      onOk={onSave}
      onCancel={onClose}
    >
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item label="节点">
          <Typography.Text strong copyable={{ text: node?.nodeId ?? "" }}>
            {displayName}
          </Typography.Text>
        </Form.Item>
        <Form.Item name="label" label="标签">
          <Input placeholder="用户标签（分组/筛选，如 工位A-桌面）" allowClear maxLength={128} />
        </Form.Item>
        <Form.Item name="location" label="位置">
          <Input placeholder="位置维度（如 SH-IDC）" allowClear maxLength={128} />
        </Form.Item>
        <Form.Item
          name="project"
          label="项目"
          extra="节点归属项目（多租户隔离）。留空=未归属（仅全域令牌可访问）。仅全域管理员可修改归属。"
        >
          <Input placeholder="team-a（留空为未归属）" allowClear maxLength={128} />
        </Form.Item>
      </Form>
    </Modal>
  );
}
