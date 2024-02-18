import { AuthGuard } from '@nestjs/passport';

export class RtGuards extends AuthGuard('jwt') {
  constructor() {
    super();
  }
}
