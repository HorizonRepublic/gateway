import { Controller } from '@nestjs/common';
import { MessagePattern } from '@nestjs/microservices';

interface IGatewayReply {
  status: number;
  headers: Record<string, string[]>;
  body: unknown;
}

const replyOf = (kind: 'echo' | 'alt'): IGatewayReply => ({
  status: 200,
  headers: { 'content-type': ['application/json'] },
  body: { ok: true, kind },
});

@Controller()
export class ShadowController {
  @MessagePattern('reload.echo')
  public echo(): IGatewayReply {
    return replyOf('echo');
  }

  @MessagePattern('reload.echo.alt')
  public echoAlt(): IGatewayReply {
    return replyOf('alt');
  }
}
